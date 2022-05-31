package hotline

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"go.uber.org/zap"
	"io"
	"io/ioutil"
	"math/big"
	"math/rand"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v2"
)

const (
	userIdleSeconds        = 300 // time in seconds before an inactive user is marked idle
	idleCheckInterval      = 10  // time in seconds to check for idle users
	trackerUpdateFrequency = 300 // time in seconds between tracker re-registration
)

type Server struct {
	Port          int
	Accounts      map[string]*Account
	Agreement     []byte
	Clients       map[uint16]*ClientConn
	FlatNews      []byte
	ThreadedNews  *ThreadedNews
	FileTransfers map[uint32]*FileTransfer
	Config        *Config
	ConfigDir     string
	Logger        *zap.SugaredLogger
	PrivateChats  map[uint32]*PrivateChat
	NextGuestID   *uint16
	TrackerPassID []byte
	Stats         *Stats

	APIListener  net.Listener
	FileListener net.Listener

	// newsReader io.Reader
	// newsWriter io.WriteCloser

	outbox chan Transaction

	mux         sync.Mutex
	flatNewsMux sync.Mutex
}

type PrivateChat struct {
	Subject    string
	ClientConn map[uint16]*ClientConn
}

func (s *Server) ListenAndServe(ctx context.Context, cancelRoot context.CancelFunc) error {
	s.Logger.Infow("Hotline server started", "version", VERSION)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { s.Logger.Fatal(s.Serve(ctx, cancelRoot, s.APIListener)) }()

	wg.Add(1)
	go func() { s.Logger.Fatal(s.ServeFileTransfers(s.FileListener)) }()

	wg.Wait()

	return nil
}

func (s *Server) APIPort() int {
	return s.APIListener.Addr().(*net.TCPAddr).Port
}

func (s *Server) ServeFileTransfers(ln net.Listener) error {
	s.Logger.Infow("Hotline file transfer server started", "Addr", fmt.Sprintf(":%v", s.Port+1))

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}

		go func() {
			if err := s.handleFileTransfer(conn); err != nil {
				s.Logger.Errorw("file transfer error", "reason", err)
			}
		}()
	}
}

func (s *Server) sendTransaction(t Transaction) error {
	requestNum := binary.BigEndian.Uint16(t.Type)
	clientID, err := byteToInt(*t.clientID)
	if err != nil {
		return err
	}

	s.mux.Lock()
	client := s.Clients[uint16(clientID)]
	s.mux.Unlock()
	if client == nil {
		return fmt.Errorf("invalid client id %v", *t.clientID)
	}
	userName := string(client.UserName)
	login := client.Account.Login

	handler := TransactionHandlers[requestNum]

	b, err := t.MarshalBinary()
	if err != nil {
		return err
	}
	var n int
	if n, err = client.Connection.Write(b); err != nil {
		return err
	}
	s.Logger.Debugw("Sent Transaction",
		"name", userName,
		"login", login,
		"IsReply", t.IsReply,
		"type", handler.Name,
		"sentBytes", n,
		"remoteAddr", client.Connection.RemoteAddr(),
	)
	return nil
}

func (s *Server) Serve(ctx context.Context, cancelRoot context.CancelFunc, ln net.Listener) error {
	s.Logger.Infow("Hotline server started", "Addr", fmt.Sprintf(":%v", s.Port))

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.Logger.Errorw("error accepting connection", "err", err)
		}

		go func() {
			for {
				t := <-s.outbox
				go func() {
					if err := s.sendTransaction(t); err != nil {
						s.Logger.Errorw("error sending transaction", "err", err)
					}
				}()
			}
		}()
		go func() {
			if err := s.handleNewConnection(conn); err != nil {
				if err == io.EOF {
					s.Logger.Infow("Client disconnected", "RemoteAddr", conn.RemoteAddr())
				} else {
					s.Logger.Errorw("error serving request", "RemoteAddr", conn.RemoteAddr(), "err", err)
				}
			}
		}()
	}
}

const (
	agreementFile = "Agreement.txt"
)

// NewServer constructs a new Server from a config dir
func NewServer(configDir, netInterface string, netPort int, logger *zap.SugaredLogger) (*Server, error) {
	server := Server{
		Port:          netPort,
		Accounts:      make(map[string]*Account),
		Config:        new(Config),
		Clients:       make(map[uint16]*ClientConn),
		FileTransfers: make(map[uint32]*FileTransfer),
		PrivateChats:  make(map[uint32]*PrivateChat),
		ConfigDir:     configDir,
		Logger:        logger,
		NextGuestID:   new(uint16),
		outbox:        make(chan Transaction),
		Stats:         &Stats{StartTime: time.Now()},
		ThreadedNews:  &ThreadedNews{},
		TrackerPassID: make([]byte, 4),
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%v", netInterface, netPort))
	if err != nil {
		return nil, err
	}
	server.APIListener = ln

	if netPort != 0 {
		netPort += 1
	}

	ln2, err := net.Listen("tcp", fmt.Sprintf("%s:%v", netInterface, netPort))
	server.FileListener = ln2
	if err != nil {
		return nil, err
	}

	// generate a new random passID for tracker registration
	if _, err := rand.Read(server.TrackerPassID); err != nil {
		return nil, err
	}

	server.Logger.Debugw("Loading Agreement", "path", configDir+agreementFile)
	if server.Agreement, err = os.ReadFile(configDir + agreementFile); err != nil {
		return nil, err
	}

	if server.FlatNews, err = os.ReadFile(configDir + "MessageBoard.txt"); err != nil {
		return nil, err
	}

	if err := server.loadThreadedNews(configDir + "ThreadedNews.yaml"); err != nil {
		return nil, err
	}

	if err := server.loadConfig(configDir + "config.yaml"); err != nil {
		return nil, err
	}

	if err := server.loadAccounts(configDir + "Users/"); err != nil {
		return nil, err
	}

	server.Config.FileRoot = configDir + "Files/"

	*server.NextGuestID = 1

	if server.Config.EnableTrackerRegistration {
		go func() {
			for {
				tr := TrackerRegistration{
					Port:        []byte{0x15, 0x7c},
					UserCount:   server.userCount(),
					PassID:      server.TrackerPassID,
					Name:        server.Config.Name,
					Description: server.Config.Description,
				}
				for _, t := range server.Config.Trackers {
					server.Logger.Infof("Registering with tracker %v", t)

					if err := register(t, tr); err != nil {
						server.Logger.Errorw("unable to register with tracker %v", "error", err)
					}
				}

				time.Sleep(trackerUpdateFrequency * time.Second)
			}
		}()
	}

	// Start Client Keepalive go routine
	go server.keepaliveHandler()

	return &server, nil
}

func (s *Server) userCount() int {
	s.mux.Lock()
	defer s.mux.Unlock()

	return len(s.Clients)
}

func (s *Server) keepaliveHandler() {
	for {
		time.Sleep(idleCheckInterval * time.Second)
		s.mux.Lock()

		for _, c := range s.Clients {
			c.IdleTime += idleCheckInterval
			if c.IdleTime > userIdleSeconds && !c.Idle {
				c.Idle = true

				flagBitmap := big.NewInt(int64(binary.BigEndian.Uint16(*c.Flags)))
				flagBitmap.SetBit(flagBitmap, userFlagAway, 1)
				binary.BigEndian.PutUint16(*c.Flags, uint16(flagBitmap.Int64()))

				c.sendAll(
					tranNotifyChangeUser,
					NewField(fieldUserID, *c.ID),
					NewField(fieldUserFlags, *c.Flags),
					NewField(fieldUserName, c.UserName),
					NewField(fieldUserIconID, *c.Icon),
				)
			}
		}
		s.mux.Unlock()
	}
}

func (s *Server) writeThreadedNews() error {
	s.mux.Lock()
	defer s.mux.Unlock()

	out, err := yaml.Marshal(s.ThreadedNews)
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(
		s.ConfigDir+"ThreadedNews.yaml",
		out,
		0666,
	)
	return err
}

func (s *Server) NewClientConn(conn net.Conn) *ClientConn {
	s.mux.Lock()
	defer s.mux.Unlock()

	clientConn := &ClientConn{
		ID:         &[]byte{0, 0},
		Icon:       &[]byte{0, 0},
		Flags:      &[]byte{0, 0},
		UserName:   []byte{},
		Connection: conn,
		Server:     s,
		Version:    &[]byte{},
		AutoReply:  []byte{},
		Transfers:  make(map[int][]*FileTransfer),
		Agreed:     false,
	}
	*s.NextGuestID++
	ID := *s.NextGuestID

	binary.BigEndian.PutUint16(*clientConn.ID, ID)
	s.Clients[ID] = clientConn

	return clientConn
}

// NewUser creates a new user account entry in the server map and config file
func (s *Server) NewUser(login, name, password string, access []byte) error {
	s.mux.Lock()
	defer s.mux.Unlock()

	account := Account{
		Login:    login,
		Name:     name,
		Password: hashAndSalt([]byte(password)),
		Access:   &access,
	}
	out, err := yaml.Marshal(&account)
	if err != nil {
		return err
	}
	s.Accounts[login] = &account

	return ioutil.WriteFile(s.ConfigDir+"Users/"+login+".yaml", out, 0666)
}

// DeleteUser deletes the user account
func (s *Server) DeleteUser(login string) error {
	s.mux.Lock()
	defer s.mux.Unlock()

	delete(s.Accounts, login)

	return FS.Remove(s.ConfigDir + "Users/" + login + ".yaml")
}

func (s *Server) connectedUsers() []Field {
	s.mux.Lock()
	defer s.mux.Unlock()

	var connectedUsers []Field
	for _, c := range sortedClients(s.Clients) {
		if !c.Agreed {
			continue
		}
		user := User{
			ID:    *c.ID,
			Icon:  *c.Icon,
			Flags: *c.Flags,
			Name:  string(c.UserName),
		}
		connectedUsers = append(connectedUsers, NewField(fieldUsernameWithInfo, user.Payload()))
	}
	return connectedUsers
}

// loadThreadedNews loads the threaded news data from disk
func (s *Server) loadThreadedNews(threadedNewsPath string) error {
	fh, err := os.Open(threadedNewsPath)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(fh)
	decoder.SetStrict(true)

	return decoder.Decode(s.ThreadedNews)
}

// loadAccounts loads account data from disk
func (s *Server) loadAccounts(userDir string) error {
	matches, err := filepath.Glob(path.Join(userDir, "*.yaml"))
	if err != nil {
		return err
	}

	if len(matches) == 0 {
		return errors.New("no user accounts found in " + userDir)
	}

	for _, file := range matches {
		fh, err := FS.Open(file)
		if err != nil {
			return err
		}

		account := Account{}
		decoder := yaml.NewDecoder(fh)
		decoder.SetStrict(true)
		if err := decoder.Decode(&account); err != nil {
			return err
		}

		s.Accounts[account.Login] = &account
	}
	return nil
}

func (s *Server) loadConfig(path string) error {
	fh, err := FS.Open(path)
	if err != nil {
		return err
	}

	decoder := yaml.NewDecoder(fh)
	decoder.SetStrict(true)
	err = decoder.Decode(s.Config)
	if err != nil {
		return err
	}
	return nil
}

const (
	minTransactionLen = 22 // minimum length of any transaction
)

// handleNewConnection takes a new net.Conn and performs the initial login sequence
func (s *Server) handleNewConnection(conn net.Conn) error {
	handshakeBuf := make([]byte, 12) // handshakes are always 12 bytes in length
	if _, err := conn.Read(handshakeBuf); err != nil {
		return err
	}
	if err := Handshake(conn, handshakeBuf[:12]); err != nil {
		return err
	}

	buf := make([]byte, 1024)
	readLen, err := conn.Read(buf)
	if readLen < minTransactionLen {
		return err
	}
	if err != nil {
		return err
	}

	clientLogin, _, err := ReadTransaction(buf[:readLen])
	if err != nil {
		return err
	}

	c := s.NewClientConn(conn)
	defer c.Disconnect()
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("stacktrace from panic: \n" + string(debug.Stack()))
			c.Server.Logger.Errorw("PANIC", "err", r, "trace", string(debug.Stack()))
			c.Disconnect()
		}
	}()

	encodedLogin := clientLogin.GetField(fieldUserLogin).Data
	encodedPassword := clientLogin.GetField(fieldUserPassword).Data
	*c.Version = clientLogin.GetField(fieldVersion).Data

	var login string
	for _, char := range encodedLogin {
		login += string(rune(255 - uint(char)))
	}
	if login == "" {
		login = GuestAccount
	}

	// If authentication fails, send error reply and close connection
	if !c.Authenticate(login, encodedPassword) {
		t := c.NewErrReply(clientLogin, "Incorrect login.")
		b, err := t.MarshalBinary()
		if err != nil {
			return err
		}
		if _, err := conn.Write(b); err != nil {
			return err
		}
		return fmt.Errorf("incorrect login")
	}

	if clientLogin.GetField(fieldUserName).Data != nil {
		c.UserName = clientLogin.GetField(fieldUserName).Data
	}

	if clientLogin.GetField(fieldUserIconID).Data != nil {
		*c.Icon = clientLogin.GetField(fieldUserIconID).Data
	}

	c.Account = c.Server.Accounts[login]

	if c.Authorize(accessDisconUser) {
		*c.Flags = []byte{0, 2}
	}

	s.Logger.Infow("Client connection received", "login", login, "version", *c.Version, "RemoteAddr", conn.RemoteAddr().String())

	s.outbox <- c.NewReply(clientLogin,
		NewField(fieldVersion, []byte{0x00, 0xbe}),
		NewField(fieldCommunityBannerID, []byte{0x00, 0x01}),
		NewField(fieldServerName, []byte(s.Config.Name)),
	)

	// Send user access privs so client UI knows how to behave
	c.Server.outbox <- *NewTransaction(tranUserAccess, c.ID, NewField(fieldUserAccess, *c.Account.Access))

	// Show agreement to client
	c.Server.outbox <- *NewTransaction(tranShowAgreement, c.ID, NewField(fieldData, s.Agreement))

	// assume simplified hotline v1.2.3 login flow that does not require agreement
	if *c.Version == nil {
		c.Agreed = true

		c.notifyOthers(
			*NewTransaction(
				tranNotifyChangeUser, nil,
				NewField(fieldUserName, c.UserName),
				NewField(fieldUserID, *c.ID),
				NewField(fieldUserIconID, *c.Icon),
				NewField(fieldUserFlags, *c.Flags),
			),
		)
	}

	c.Server.Stats.LoginCount += 1

	const readBuffSize = 1024000 // 1KB - TODO: what should this be?
	tranBuff := make([]byte, 0)
	tReadlen := 0
	// Infinite loop where take action on incoming client requests until the connection is closed
	for {
		buf = make([]byte, readBuffSize)
		tranBuff = tranBuff[tReadlen:]

		readLen, err := c.Connection.Read(buf)
		if err != nil {
			return err
		}
		tranBuff = append(tranBuff, buf[:readLen]...)

		// We may have read multiple requests worth of bytes from Connection.Read.  readTransactions splits them
		// into a slice of transactions
		var transactions []Transaction
		if transactions, tReadlen, err = readTransactions(tranBuff); err != nil {
			c.Server.Logger.Errorw("Error handling transaction", "err", err)
		}

		// iterate over all of the transactions that were parsed from the byte slice and handle them
		for _, t := range transactions {
			if err := c.handleTransaction(&t); err != nil {
				c.Server.Logger.Errorw("Error handling transaction", "err", err)
			}
		}
	}
}

// NewTransactionRef generates a random ID for the file transfer.  The Hotline client includes this ID
// in the file transfer request payload, and the file transfer server will use it to map the request
// to a transfer
func (s *Server) NewTransactionRef() []byte {
	transactionRef := make([]byte, 4)
	rand.Read(transactionRef)

	return transactionRef
}

func (s *Server) NewPrivateChat(cc *ClientConn) []byte {
	s.mux.Lock()
	defer s.mux.Unlock()

	randID := make([]byte, 4)
	rand.Read(randID)
	data := binary.BigEndian.Uint32(randID[:])

	s.PrivateChats[data] = &PrivateChat{
		Subject:    "",
		ClientConn: make(map[uint16]*ClientConn),
	}
	s.PrivateChats[data].ClientConn[cc.uint16ID()] = cc

	return randID
}

const dlFldrActionSendFile = 1
const dlFldrActionResumeFile = 2
const dlFldrActionNextFile = 3

// handleFileTransfer receives a client net.Conn from the file transfer server, performs the requested transfer type, then closes the connection
func (s *Server) handleFileTransfer(conn io.ReadWriteCloser) error {
	defer func() {
		if err := conn.Close(); err != nil {
			s.Logger.Errorw("error closing connection", "error", err)
		}
	}()

	txBuf := make([]byte, 16)
	_, err := conn.Read(txBuf)
	if err != nil {
		return err
	}

	var t transfer
	_, err = t.Write(txBuf)
	if err != nil {
		return err
	}

	transferRefNum := binary.BigEndian.Uint32(t.ReferenceNumber[:])
	fileTransfer := s.FileTransfers[transferRefNum]

	switch fileTransfer.Type {
	case FileDownload:
		fullFilePath, err := readPath(s.Config.FileRoot, fileTransfer.FilePath, fileTransfer.FileName)
		if err != nil {
			return err
		}

		ffo, err := NewFlattenedFileObject(
			s.Config.FileRoot,
			fileTransfer.FilePath,
			fileTransfer.FileName,
		)
		if err != nil {
			return err
		}

		s.Logger.Infow("File download started", "filePath", fullFilePath, "transactionRef", fileTransfer.ReferenceNumber)

		// Start by sending flat file object to client
		if _, err := conn.Write(ffo.BinaryMarshal()); err != nil {
			return err
		}

		file, err := FS.Open(fullFilePath)
		if err != nil {
			return err
		}

		sendBuffer := make([]byte, 1048576)
		for {
			var bytesRead int
			if bytesRead, err = file.Read(sendBuffer); err == io.EOF {
				break
			}

			fileTransfer.BytesSent += bytesRead

			delete(s.FileTransfers, transferRefNum)

			if _, err := conn.Write(sendBuffer[:bytesRead]); err != nil {
				return err
			}
		}
	case FileUpload:
		destinationFile := s.Config.FileRoot + ReadFilePath(fileTransfer.FilePath) + "/" + string(fileTransfer.FileName)
		newFile, err := FS.Create(destinationFile)
		if err != nil {
			return err
		}
		defer func() { _ = newFile.Close() }()

		s.Logger.Infow("File upload started", "transactionRef", fileTransfer.ReferenceNumber, "dstFile", destinationFile)

		if err := receiveFile(conn, newFile, nil); err != nil {
			s.Logger.Errorw("file upload error", "error", err)
		}

		s.Logger.Infow("File upload complete", "transactionRef", fileTransfer.ReferenceNumber, "dstFile", destinationFile)
	case FolderDownload:
		// Folder Download flow:
		// 1. Get filePath from the transfer
		// 2. Iterate over files
		// 3. For each file:
		// 	 Send file header to client
		// The client can reply in 3 ways:
		//
		// 1. If type is an odd number (unknown type?), or file download for the current file is completed:
		//		client sends []byte{0x00, 0x03} to tell the server to continue to the next file
		//
		// 2. If download of a file is to be resumed:
		//		client sends:
		//			[]byte{0x00, 0x02} // download folder action
		//			[2]byte // Resume data size
		//			[]byte file resume data (see myField_FileResumeData)
		//
		// 3. Otherwise download of the file is requested and client sends []byte{0x00, 0x01}
		//
		// When download is requested (case 2 or 3), server replies with:
		// 			[4]byte - file size
		//			[]byte  - Flattened File Object
		//
		// After every file download, client could request next file with:
		// 			[]byte{0x00, 0x03}
		//
		// This notifies the server to send the next item header

		fullFilePath, err := readPath(s.Config.FileRoot, fileTransfer.FilePath, fileTransfer.FileName)
		if err != nil {
			return err
		}

		basePathLen := len(fullFilePath)

		s.Logger.Infow("Start folder download", "path", fullFilePath, "ReferenceNumber", fileTransfer.ReferenceNumber)

		nextAction := make([]byte, 2)
		if _, err := conn.Read(nextAction); err != nil {
			return err
		}

		i := 0
		err = filepath.Walk(fullFilePath+"/", func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			i += 1
			subPath := path[basePathLen+1:]
			s.Logger.Infow("Sending fileheader", "i", i, "path", path, "fullFilePath", fullFilePath, "subPath", subPath, "IsDir", info.IsDir())

			if i == 1 {
				return nil
			}

			fileHeader := NewFileHeader(subPath, info.IsDir())

			// Send the file header to client
			if _, err := conn.Write(fileHeader.Payload()); err != nil {
				s.Logger.Errorf("error sending file header: %v", err)
				return err
			}

			// Read the client's Next Action request
			if _, err := conn.Read(nextAction); err != nil {
				return err
			}
			if nextAction[1] == 3 {
				return nil
			}

			s.Logger.Infow("Client folder download action", "action", fmt.Sprintf("%X", nextAction[0:2]))

			if info.IsDir() {
				return nil
			}

			splitPath := strings.Split(path, "/")

			ffo, err := NewFlattenedFileObject(
				strings.Join(splitPath[:len(splitPath)-1], "/"),
				nil,
				[]byte(info.Name()),
			)
			if err != nil {
				return err
			}
			s.Logger.Infow("File download started",
				"fileName", info.Name(),
				"transactionRef", fileTransfer.ReferenceNumber,
				"TransferSize", fmt.Sprintf("%x", ffo.TransferSize()),
			)

			// Send file size to client
			if _, err := conn.Write(ffo.TransferSize()); err != nil {
				s.Logger.Error(err)
				return err
			}

			// Send ffo bytes to client
			if _, err := conn.Write(ffo.BinaryMarshal()); err != nil {
				s.Logger.Error(err)
				return err
			}

			file, err := FS.Open(path)
			if err != nil {
				return err
			}

			// Copy N bytes from file to connection
			_, err = io.CopyN(conn, file, int64(binary.BigEndian.Uint32(ffo.FlatFileDataForkHeader.DataSize[:])))
			if err != nil {
				return err
			}
			file.Close()

			// TODO: optionally send resource fork header and resource fork data

			// Read the client's Next Action request
			if _, err := conn.Read(nextAction); err != nil {
				return err
			}
			// TODO: switch behavior based on possible next action

			return err
		})

	case FolderUpload:
		dstPath, err := readPath(s.Config.FileRoot, fileTransfer.FilePath, fileTransfer.FileName)
		if err != nil {
			return err
		}
		s.Logger.Infow(
			"Folder upload started",
			"transactionRef", fileTransfer.ReferenceNumber,
			"dstPath", dstPath,
			"TransferSize", fmt.Sprintf("%x", fileTransfer.TransferSize),
			"FolderItemCount", fileTransfer.FolderItemCount,
		)

		// Check if the target folder exists.  If not, create it.
		if _, err := FS.Stat(dstPath); os.IsNotExist(err) {
			s.Logger.Infow("Creating target path", "dstPath", dstPath)
			if err := FS.Mkdir(dstPath, 0777); err != nil {
				s.Logger.Error(err)
			}
		}

		// Begin the folder upload flow by sending the "next file action" to client
		if _, err := conn.Write([]byte{0, dlFldrActionNextFile}); err != nil {
			return err
		}

		fileSize := make([]byte, 4)
		itemCount := binary.BigEndian.Uint16(fileTransfer.FolderItemCount)

		readBuffer := make([]byte, 1024)
		for i := uint16(0); i < itemCount; i++ {
			_, err := conn.Read(readBuffer)
			if err != nil {
				return err
			}
			fu := readFolderUpload(readBuffer)

			s.Logger.Infow(
				"Folder upload continued",
				"transactionRef", fmt.Sprintf("%x", fileTransfer.ReferenceNumber),
				"FormattedPath", fu.FormattedPath(),
				"IsFolder", fmt.Sprintf("%x", fu.IsFolder),
				"PathItemCount", binary.BigEndian.Uint16(fu.PathItemCount[:]),
			)

			if fu.IsFolder == [2]byte{0, 1} {
				if _, err := os.Stat(dstPath + "/" + fu.FormattedPath()); os.IsNotExist(err) {
					s.Logger.Infow("Target path does not exist; Creating...", "dstPath", dstPath)
					if err := os.Mkdir(dstPath+"/"+fu.FormattedPath(), 0777); err != nil {
						s.Logger.Error(err)
					}
				}

				// Tell client to send next file
				if _, err := conn.Write([]byte{0, dlFldrActionNextFile}); err != nil {
					s.Logger.Error(err)
					return err
				}
			} else {
				// TODO: Check if we have the full file already.  If so, send dlFldrAction_NextFile to client to skip.
				// TODO: Check if we have a partial file already.  If so, send dlFldrAction_ResumeFile to client to resume upload.
				// Send dlFldrAction_SendFile to client to begin transfer
				if _, err := conn.Write([]byte{0, dlFldrActionSendFile}); err != nil {
					return err
				}

				if _, err := conn.Read(fileSize); err != nil {
					return err
				}

				filePath := dstPath + "/" + fu.FormattedPath()
				s.Logger.Infow("Starting file transfer", "path", filePath, "fileNum", i+1, "totalFiles", itemCount, "fileSize", binary.BigEndian.Uint32(fileSize))

				newFile, err := FS.Create(filePath)
				if err != nil {
					return err
				}

				if err := receiveFile(conn, newFile, ioutil.Discard); err != nil {
					s.Logger.Error(err)
				}
				_ = newFile.Close()

				// Tell client to send next file
				if _, err := conn.Write([]byte{0, dlFldrActionNextFile}); err != nil {
					s.Logger.Error(err)
					return err
				}

			}
		}
		s.Logger.Infof("Folder upload complete")
	}

	return nil
}

// sortedClients is a utility function that takes a map of *ClientConn and returns a sorted slice of the values.
// The purpose of this is to ensure that the ordering of client connections is deterministic so that test assertions work.
func sortedClients(unsortedClients map[uint16]*ClientConn) (clients []*ClientConn) {
	for _, c := range unsortedClients {
		clients = append(clients, c)
	}
	sort.Sort(byClientID(clients))
	return clients
}

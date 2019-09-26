package main

import (
	"bufio"
	"fmt"
	"github.com/atotto/clipboard"
	"github.com/op/go-logging"
	"github.com/pingcap/errors"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"
)

const (
	ServerAddr    = "10.0.2.2"
	ListenPort    = 61234
	SingleInstancePort = 61235
	ConnType      = "tcp4"
	FlagDebug     = "debug"
	FlagClient    = "client"
	DelimiterByte = byte(14)
	AllowedNetwork = "127.0.0.1"
)
var(
	connected bool
	serverMode bool
	clipboardContent string
	clipboardContentLock sync.RWMutex
	connectedLock sync.RWMutex
	clientMap map[net.Addr]net.Conn
)
var log = logging.MustGetLogger("CLIPBOARD-SYNC")
func setConnected(val bool)  {
	connectedLock.Lock()
	defer connectedLock.Unlock()
	connected = val
}

func getConnected() bool {
	connectedLock.RLock()
	defer connectedLock.RUnlock()
	return connected
}

func getClipboardContent() string {
	clipboardContentLock.RLock()
	defer clipboardContentLock.RUnlock()
	return clipboardContent
}
func setClipboardContent(content string)  {
	clipboardContentLock.Lock()
	defer clipboardContentLock.Unlock()
	clipboardContent = content
}

func search(searchIn []string, toSearch string) bool {
	for _, content := range searchIn {
		if content == toSearch {
			return true
		}
	}
	return false
}

func readCommandArguments() (debug bool, client bool)  {
	args := os.Args
	clientMode := search(args, FlagClient)
	debugMode := search(args, FlagDebug)
	exeName := args[0]
	parts := strings.Split(exeName, ".")

	if !debugMode {
		debugMode = search(parts, FlagDebug)
	}

	if !clientMode {
		clientMode = search(parts, FlagClient)
	}

	return debugMode, clientMode
}
func isClosedErr(err error) bool {
	return err == io.EOF || strings.Contains(err.Error(), "closed network");
}
func handleConnectionRead(c net.Conn, recvChannel chan string, closeChannel chan struct{}) {
	log.Infof("%s connected", c.RemoteAddr().String())
	for {
		rawBytes, err := bufio.NewReader(c).ReadBytes(DelimiterByte)
		if nil != err {
			log.Errorf("Read content from client failed %v", err)
			if isClosedErr(err) {
				log.Error("Disconnected")
				delete(clientMap, c.RemoteAddr())
				_ = c.Close()
				if  nil != closeChannel {
					close(closeChannel)
				}
				break
			}
			return
		}
		content := strings.TrimSpace(string(rawBytes[0 : len(rawBytes) - 1]))
		log.Debugf("Received clipboard content %s", content)
		if len(content) > 0 {
			recvChannel <- content
		}
	}
}

func handleConnectionWrite(conn net.Conn, senderChannel chan string, closeChannel chan struct{}) {
	for{
		content := <- senderChannel
		if len(content) > 0 {
			c, err := conn.Write([] byte(content))
			_, err = conn.Write([]byte{DelimiterByte})
			if nil != err {
				log.Errorf("Failed to send clipboard to remote host: %v", err)
				if isClosedErr(err) {
					log.Error("Disconnected")
					delete(clientMap, conn.RemoteAddr())
					_ = conn.Close()
					if  nil != closeChannel {
						close(closeChannel)
					}
					break
				}
				continue
			}
			log.Debugf("Sent %d bytes to remote: %s", c, content)
		}
	}
}

/**
启动服务器
 */
func startServer(listenHost string, port int32, recvChannel chan string, senderChannel chan string) {
	log.Infof("Starting server...")
	log.Infof("Listening port %v at %v", listenHost, port)
	listenAddress := fmt.Sprintf("%s%d", listenHost, port)
	ln, err := net.Listen(ConnType, listenAddress)
	if nil != err {
		log.Fatal(err)
	}
	defer ln.Close()
	serverChannel := make(chan struct{})
	defer close(senderChannel)
	allowedAddress := strings.Split(AllowedNetwork, ",")
	go func() {
		for{
			c, err := ln.Accept()
			if err != nil {
				log.Errorf("Failed to accept connection %v", err)
				continue
			}
			network := c.RemoteAddr().String()
			var ok = false
			for _, line := range allowedAddress {
				if strings.HasPrefix(network, line) {
					ok = true
				}
			}
			if !ok{
				log.Error("Only allowed connection from 127.0.0.1")
				_ = c.Close()
				continue
			}
			setConnected(true)
			// save the client reference
			clientMap[c.RemoteAddr()] = c
			go handleConnectionRead(c, recvChannel,nil)
			go handleConnectionWrite(c, senderChannel, nil)
		}
	}()
	<- serverChannel
}

func startClient(serverHost string, port int32, recvChannel chan string, sendChannel chan string)  {
	for {
		setConnected(false)
		address := fmt.Sprintf("%s:%d", serverHost, port)
		log.Infof("About to connect server %s", address)
		conn, err  := net.Dial(ConnType, address)
		if nil != err {
			log.Errorf("Failed to connect %s, retrying... ", err)
			continue
		}
		setConnected(true)
		closeChannel := make(chan struct{})
		go handleConnectionRead(conn, recvChannel, closeChannel)
		go handleConnectionWrite(conn, sendChannel, closeChannel)
		<- closeChannel
	}
}

func handleClipboardReceived(recvChannel chan string)  {
	for {
		newContent := <-recvChannel
		oldContent := getClipboardContent()
		if newContent != oldContent {
			oldContent = newContent
			err := clipboard.WriteAll(newContent)
			setClipboardContent(newContent)
			if nil != err {
				log.Errorf("Failed to apply clipboard content %s: %v", newContent, err)
			} else {
				log.Debugf("Applied clipboard content: [%s]", newContent)
			}
		}
	}
}

func monitorLocalClipboard(sendChanel chan string) {
	oldContent, err := clipboard.ReadAll()
	if nil != err {
		log.Errorf("Read clipboard failed %v", err)
		oldContent = ""
	}
	setClipboardContent(oldContent)
	log.Infof("Started monitoring local clipboard")
	for {
		time.Sleep(time.Millisecond * 200)
		newContent, err := clipboard.ReadAll()
		if nil != err {
			continue
		}
		oldContent = getClipboardContent()
		if len(newContent) > 0 && newContent != oldContent {
			oldContent = newContent
			setClipboardContent(newContent)
			// client connected OR server with client connection(s)
			if (getConnected() && !serverMode) || (serverMode && len(clientMap) > 0) {
				log.Debugf("Send local clipboard change: %s", newContent)
				sendChanel <- newContent
			} else {
				log.Debugf("Not connected, no need to send change: %s", newContent)
			}
		}
	}
}

func setupLogging(debug bool) *os.File {
	var format = logging.MustStringFormatter(
		`%{time:15:04:05.000} %{shortfunc}  %{level:.4s} %{id:03x} %{message}`,
	)
	logFile, err := os.OpenFile("clipboard.log", os.O_RDWR | os.O_CREATE, 0664)
	if err != nil {
		fmt.Print("Failed to open log file")
	}
	infoBackend := logging.NewLogBackend(os.Stdout, "", 0)
	fileBackend := logging.NewLogBackend(logFile, "", 0)
	errorBackend := logging.NewLogBackend(os.Stderr, "", 0)
	errorFormatter := logging.NewBackendFormatter(errorBackend, format)


	defaultBackendLevel := logging.AddModuleLevel(infoBackend)
	logFileBackendLevel := logging.AddModuleLevel(fileBackend)
	if debug{
		defaultBackendLevel.SetLevel(logging.DEBUG, "")
		logFileBackendLevel.SetLevel(logging.DEBUG, "")
	} else {
		defaultBackendLevel.SetLevel(logging.WARNING, "")
		logFileBackendLevel.SetLevel(logging.WARNING, "")
	}

	logging.SetBackend(defaultBackendLevel, errorFormatter, logFileBackendLevel)
	return logFile
}

func singleInstance() error {
	listenAddress := fmt.Sprintf(":%d", SingleInstancePort)
	if _, err := net.Listen(ConnType, listenAddress); err != nil {
		return errors.New("Another instance were running" )
	}
	return nil
}

func main()  {
	debug, client := readCommandArguments()
	serverMode = !client
	logFile := setupLogging(debug)
	if nil != logFile{
		defer func() {
			_ = logFile.Close()
		}()
	}
	if nil != singleInstance() {
		log.Fatalf("Already running")
		os.Exit(1)
	}
	log.Debugf("debug ? %v, client ? %v ", debug, client)
	// Channel for receiving remote clipboard
	recvChannel := make(chan string)
	// Channel for sending clipboard content to remote
	sendChannel := make(chan string)
	// Connected clients
	clientMap = make(map[net.Addr]net.Conn)
	// Monitoring change for clipboard
	go monitorLocalClipboard(sendChannel)
	// Server/Client startup
	if !client {
		go startServer(":", ListenPort, recvChannel, sendChannel)
	} else {
		go startClient(ServerAddr, ListenPort, recvChannel, sendChannel)
	}
	// Applying clipboard content from remote
	go handleClipboardReceived(recvChannel)

	// process abort signal
	signalChan := make(chan os.Signal, 1)
	cleanupDone := make(chan struct{})
	signal.Notify(signalChan, os.Interrupt)
	go func() {
		<-signalChan
		log.Error("aborted")
		close(cleanupDone)
	}()
	<-cleanupDone
}


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
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync"
	"time"
)

const (
	ServerAddr         = "10.0.2.2"
	ListenPort         = 61234
	SingleInstancePort = 61235
	ConnType           = "tcp4"
	FlagDebug          = "debug"
	FlagClient         = "client"
	DelimiterByte      = byte(14)
	AllowedNetwork     = "127.0.0.1"
)

var (
	connected                bool
	serverMode               bool
	clipboardContent         string
	clipboardContentLock     sync.RWMutex
	connectedLock            sync.RWMutex
	connectionLostStatusLock sync.RWMutex
	clientMap                map[net.Addr]net.Conn
	clientChannelMap         map[net.Addr]chan string
	connectionLostStatusMap  map[int]bool
)

var log = logging.MustGetLogger("CLIPBOARD-SYNC")

func setConnected(val bool) {
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
func setClipboardContent(content string) {
	clipboardContentLock.Lock()
	defer clipboardContentLock.Unlock()
	clipboardContent = content
}

func getConnectionLostStatus(no int) bool {
	connectionLostStatusLock.RLock()
	defer connectionLostStatusLock.RUnlock()
	return connectionLostStatusMap[no]
}
func setConnectionLostStatus(no int, status bool) bool {
	connectionLostStatusLock.Lock()
	defer connectionLostStatusLock.Unlock()
	oldValue, ok := connectionLostStatusMap[no]
	if !ok {
		connectionLostStatusMap[no] = status
		return true
	}
	if oldValue != status {
		connectionLostStatusMap[no] = status
		return true
	}
	return false
}

func search(searchIn []string, toSearch string) bool {
	for _, content := range searchIn {
		if content == toSearch {
			return true
		}
	}
	return false
}

func readCommandArguments() (debug bool, client bool) {
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
	return err == io.EOF ||
		strings.Contains(err.Error(), "closed network") ||
		strings.Contains(err.Error(), "An existing connection was forcibly closed by the remote host.")
}
func readFromRemote(c net.Conn, recvChannel chan string, closeChannel chan struct{}, no int) {
	log.Infof("%s connected", c.RemoteAddr().String())
	for {
		rawBytes, err := bufio.NewReader(c).ReadBytes(DelimiterByte)
		if nil != err {
			log.Errorf("Read content from client failed %v", err)
			if isClosedErr(err) {
				log.Error("Disconnected")
				delete(clientMap, c.RemoteAddr())
				if nil != closeChannel && !getConnectionLostStatus(no) {
					_ = c.Close()
				}
				if setConnectionLostStatus(no, true) && nil != closeChannel {
					close(closeChannel)
				}
				break
			}
			continue
		}
		content := strings.TrimSpace(string(rawBytes[0 : len(rawBytes)-1]))
		if len(content) > 0 {
			log.Debugf("Received clipboard content(%d) %s", len(content), content)
			recvChannel <- content
		}
		if getConnectionLostStatus(no) {
			break
		}
	}
}

func sendToRemote(conn net.Conn, senderChannel chan string, closeChannel chan struct{}, no int) {
	for {
		content := <-senderChannel
		log.Debugf("About to sending content '%v' to %v", content, conn.RemoteAddr())
		if getConnectionLostStatus(no) {
			log.Errorf("%v was not connected", conn.RemoteAddr())
			break
		}
		if len(content) > 0 {
			c, err := conn.Write([] byte(content))
			_, err = conn.Write([]byte{DelimiterByte})
			if nil != err {
				log.Errorf("Failed to send clipboard to remote host: %v", err)
				if isClosedErr(err) {
					log.Error("Remote connection disconnected")
					delete(clientMap, conn.RemoteAddr())
					clientChannel := clientChannelMap[conn.RemoteAddr()]
					if nil != clientChannel {
						close(clientChannel)
					}
					delete(clientChannelMap, conn.RemoteAddr())
					if nil != closeChannel && !getConnectionLostStatus(no) {
						_ = conn.Close()
					}
					if setConnectionLostStatus(no, true) && nil != closeChannel {
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
func clientKeepAlive(conn net.Conn, closeChannel chan struct{}, no int) {
	data := []byte(" " + string(DelimiterByte))
	for {
		_, err := conn.Write(data)
		if nil != err {
			log.Error("Failed to send keep alive package: %v", err)
			if isClosedErr(err) {
				log.Error("Lost remote connection")
			}
			delete(clientMap, conn.RemoteAddr())
			clientChannel := clientChannelMap[conn.RemoteAddr()]
			if nil != clientChannel {
				close(clientChannel)
			}
			if nil != closeChannel && !getConnectionLostStatus(no) {
				_ = conn.Close()
			}
			if setConnectionLostStatus(no, true) && nil != closeChannel {
				close(closeChannel)
			}
			break
		}
		time.Sleep(time.Millisecond * 200)
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
		number := 0
		for {
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
			if !ok {
				log.Error("Only allowed connection from 127.0.0.1")
				_ = c.Close()
				continue
			}
			number++
			setConnected(true)
			// save the client reference
			clientMap[c.RemoteAddr()] = c
			clientChannel := make(chan string)
			clientChannelMap[c.RemoteAddr()] = clientChannel
			setConnectionLostStatus(number, false)
			go readFromRemote(c, recvChannel, nil, number)
			go sendToRemote(c, clientChannel, nil, number)
		}
	}()
	//向所有客户端发送剪贴板
	go func() {
		for {
			content := <-senderChannel
			for _, v := range clientChannelMap {
				log.Debugf("Sending content '%v' to %v \n", content, v)
				v <- content
			}
		}
	}()
	<-serverChannel
}

func startClient(serverHost string, port int32, recvChannel chan string, sendChannel chan string) {
	number := 0
	for {
		setConnected(false)
		address := fmt.Sprintf("%s:%d", serverHost, port)
		log.Infof("About to connect server %s", address)
		conn, err := net.Dial(ConnType, address)
		if nil != err {
			log.Errorf("Failed to connect %s, retrying... ", err)
			continue
		}
		number++
		setConnected(true)
		closeChannel := make(chan struct{})
		setConnectionLostStatus(number, false)
		go readFromRemote(conn, recvChannel, closeChannel, number)
		go sendToRemote(conn, sendChannel, closeChannel, number)
		go clientKeepAlive(conn, closeChannel, number)
		<-closeChannel
		// mark the connection was disconnected
		setConnectionLostStatus(number, true)
	}
}

func handleClipboardReceived(recvChannel chan string) {
	defer func() {
		log.Errorf("Restarted the goroutine handleClipboardReceived")
		go handleClipboardReceived(recvChannel)
	}()
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
func isGeneralClipboardReadError(err error) bool {
	return strings.Contains(fmt.Sprintf("%v", err), "The operation completed successfully");
}
func timeoutFiveSeconds(channel chan struct {
	bool;
	uint64
}, counter uint64) {
	time.Sleep(time.Second * 5)
	channel <- struct {
		bool
		uint64
	}{bool: true, uint64: counter}
}
func readClipboardContent(channel chan struct {
	string;
	uint64
}, counter uint64) {
	newContent, err := clipboard.ReadAll()
	if nil != err && !isGeneralClipboardReadError(err) {
		log.Errorf("Failed to read clipboard content: %v", err)
		return
	}
	channel <- struct {
		string
		uint64
	}{string: newContent, uint64: counter}
}
func monitorLocalClipboard(sendChanel chan string) {
	/**
	never quit
	*/

	defer func() {
		log.Errorf("Restarted the goroutine of monitorLocalClipboard")
		go monitorLocalClipboard(sendChanel)
	}()
	oldContent, err := clipboard.ReadAll()
	if nil != err && !isGeneralClipboardReadError(err) {
		log.Errorf("Read clipboard failed %v", err)
		oldContent = ""
	}
	setClipboardContent(oldContent)
	timeoutChannel := make(chan struct {
		bool;
		uint64
	}, 1)
	clipboardContentChannel := make(chan struct {
		string;
		uint64
	}, 1)
	log.Infof("Started monitoring local clipboard")
	counter := uint64(0)
	for {
		time.Sleep(200 * time.Millisecond)
		counter = counter + 1
		go timeoutFiveSeconds(timeoutChannel, counter)
		go readClipboardContent(clipboardContentChannel, counter)
		//handling command timeout for reading clipboard content
		select {
		case timeoutObj := <-timeoutChannel:
			if timeoutObj.uint64 == counter {
				log.Errorf("Timeout when read from clipboard: " + time.Now().Format("2006-01-02 15:04:05"))
			}
		case newContentObj := <-clipboardContentChannel:
			if newContentObj.uint64 != counter {
				continue
			}
			newContent := newContentObj.string
			oldContent = getClipboardContent()
			if len(newContent) > 0 && newContent != oldContent {
				oldContent = newContent
				setClipboardContent(newContent)
				if runtime.GOOS == "windows" {
					_ = clipboard.WriteAll(oldContent)
				}
				// client connected OR server with client connection(s)
				log.Debugf("Is server mode ? %v; Connected ? %v; Size of client map %v", serverMode, getConnected(), len(clientMap))
				if (getConnected() && !serverMode) || (serverMode && len(clientMap) > 0) {
					log.Debugf("Send local clipboard change: %s", newContent)
					sendChanel <- newContent
				} else {
					log.Debugf("Not connected, no need to send change: %s", newContent)
				}
			}
		}
	}
}

func setupLogging(debug bool) *os.File {
	var format = logging.MustStringFormatter(
		` %{time:15:04:05.000} %{shortfunc}  %{level:.4s} %{id:03x} %{message}`,
	)
	folder, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	fileName := filepath.Base(os.Args[0])
	logFilePath := filepath.Join(folder, fileName+"-output.log")
	logFile, err := os.OpenFile(logFilePath, os.O_RDWR|os.O_CREATE, 0664)
	if err != nil {
		fmt.Print("Failed to open log file")
	}
	println("Will write log to ", logFilePath)
	stdoutBackend := logging.NewLogBackend(os.Stdout, "", 0)
	fileBackend := logging.NewLogBackend(logFile, "", 0)
	errorBackend := logging.NewLogBackend(logFile, "", 0)
	errorFormatter := logging.NewBackendFormatter(errorBackend, format)

	logging.NewBackendFormatter(stdoutBackend, format)
	logging.NewBackendFormatter(fileBackend, format)

	defaultBackendLevel := logging.AddModuleLevel(stdoutBackend)
	logFileBackendLevel := logging.AddModuleLevel(fileBackend)
	errorBackendLevel := logging.AddModuleLevel(errorBackend)
	if debug {
		defaultBackendLevel.SetLevel(logging.DEBUG, "")
		logFileBackendLevel.SetLevel(logging.DEBUG, "")
		errorBackendLevel.SetLevel(logging.DEBUG, "")
	} else {
		defaultBackendLevel.SetLevel(logging.WARNING, "")
		logFileBackendLevel.SetLevel(logging.WARNING, "")
		errorBackendLevel.SetLevel(logging.WARNING, "")

	}
	logging.SetBackend(defaultBackendLevel, errorFormatter, logFileBackendLevel)
	return logFile
}

func singleInstance() error {
	listenAddress := fmt.Sprintf(":%d", SingleInstancePort)
	if _, err := net.Listen(ConnType, listenAddress); err != nil {
		return errors.New("Another instance were running")
	}
	return nil
}
func startProfile() {

	defer func() {

	}()

}
func main() {
	debug, client := readCommandArguments()
	serverMode = !client
	logFile := setupLogging(debug)
	if nil != logFile {
		defer func() {
			_ = logFile.Close()
		}()
	}
	if nil != singleInstance() {
		log.Fatalf("Already running")
		os.Exit(1)
	}
	log.Debugf("debug ? %v, client ? %v ", debug, client)
	log.Infof("Startup pprof")
	folder, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	cpuProfile, _ := os.Create(filepath.Join(folder, "cpuProfile"))
	ramProfile, _ := os.Create(filepath.Join("ramProfile"))
	// Channel for receiving remote clipboard
	recvChannel := make(chan string)
	// Channel for sending clipboard content to remote
	sendChannel := make(chan string)
	// Connected clients
	clientMap = make(map[net.Addr]net.Conn)
	clientChannelMap = make(map[net.Addr]chan string)
	connectionLostStatusMap = make(map[int]bool)
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
	err := pprof.StartCPUProfile(cpuProfile);
	if err != nil {
		log.Errorf("Failed to startup cpu profiling: %v", err)
	}
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
	pprof.StopCPUProfile()
	err = pprof.WriteHeapProfile(ramProfile)
	if nil != err {
		log.Errorf("Failed to save memory profile")
	}
}

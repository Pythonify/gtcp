package gtcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const (
	headerLen = 4
)

// All TCPConn Type should implement TCPConnInterface
type TCPConnInterface interface {
	net.Conn
	ReadData() []byte
	ReadString() string
	CloseRead() error
	CloseWrite() error
	File() (f *os.File, err error)
	ReadFrom(r io.Reader) (int64, error)
	SetKeepAlive(keepalive bool) error
	SetKeepAlivePeriod(d time.Duration) error
	SetLinger(sec int) error
	SetNoDelay(noDelay bool) error
	SetReadBuffer(bytes int) error
	SetWriteBuffer(bytes int) error
	InstallCtx(ctx context.Context)
	GetDataChan() <-chan []byte
	GetInfoChan() <-chan string
	GetErrChan() <-chan error
	Scan()
	Start()
	StartWithCtx(ctx context.Context)
	Done() <-chan struct{}
	IsDone() bool
	ReInstallNetConn(conn *net.TCPConn)
	CloseOnce()
}

// All struct with a TCPConn-Type member should implement TCPBox Interface
type TCPBox interface {
	TCPConnInterface
	InstallTCPConn(conn *TCPConn)
}

// initial a new TCPConn and return its pointer
func NewTCPConn(conn *net.TCPConn) *TCPConn {
	//ctx, cancelFunc := context.WithCancel(context.Background())
	return &TCPConn{
		data:    make(chan []byte),
		info:    make(chan string),
		error:   make(chan error),
		TCPConn: conn,
		mu:      new(sync.RWMutex),
		//Context: ctx,
		//cancel:  cancelFunc,
	}
}

// if Pool or ConnPool is open, and there are some TCPConn in them, get a TCPConn pointer from it
// else return a new TCPConn
func GetTCPConn(conn *net.TCPConn) (tcpConn *TCPConn) {
	tcpConn, ok := GetConnFromPool(conn)
	if !ok {
		tcpConn = NewTCPConn(conn)
	}
	return
}

// A box for net.TCPConn
// with data chan, info chan, error chan, ctx and cancel function.
type TCPConn struct {
	data    chan []byte
	info    chan string
	error   chan error
	mu      *sync.RWMutex
	*net.TCPConn
	Context context.Context
	cancel  context.CancelFunc
}

// Go Scan
func (t *TCPConn) Start() {
	t.InstallCtx(context.Background())
	go t.Scan()
}

// Go Scan with father ctx
func (t *TCPConn) StartWithCtx(ctx context.Context) {
	t.InstallCtx(ctx)
	go t.Scan()
}

// Executed only once in a life cycle
func (t *TCPConn) CloseOnce() {
	t.Close()
	err := t.TCPConn.Close()
	if err != nil {
		t.error <- err
	}
	SendConnToPool(t)
}

// Replace the net.TCPConn when TCPConn is recycled.
func (t *TCPConn) ReInstallNetConn(conn *net.TCPConn) {
	if !t.IsDone() {
		panic(errors.New("Unclosed TCPConn Cannot reinstall conn!"))
	}
	t.TCPConn = conn
}

// Return t.Ctx.Done()
func (t *TCPConn) Done() <-chan struct{} {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Context.Done()
}

// return true if TCPConn is closed, else false.
func (t *TCPConn) IsDone() bool {
	done := t.Done()
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// Get len of data and add it at the header of data by 4 bytes.
// then execute t.TCPConn.Write(data)
func (t *TCPConn) Write(data []byte) (int, error) {
	length := uint32(len(data))
	head := make([]byte, 4)
	binary.LittleEndian.PutUint32(head, length)
	buf := bytes.NewBuffer(head)
	buf.Write(data)
	n, err := t.TCPConn.Write(buf.Bytes())
	if err != nil {
		return n, err
	}
	return n - headerLen, nil
}

// Close TCPConn
func (t *TCPConn) Close() error {
	t.cancel()
	return nil
}

// receive a father ctx and get son WithCancel as ctx, cancel of TCPConn
func (t *TCPConn) InstallCtx(ctx context.Context) {
	if t.cancel != nil {
		t.cancel()
	}
	t.mu.Lock()
	t.Context, t.cancel = context.WithCancel(ctx)
	t.mu.Unlock()
}

// extract payload from TCP streams
func (t *TCPConn) split(data []byte, atEOF bool) (adv int, token []byte, err error) {
	length := len(data)
	if length < headerLen {
		return 0, nil, nil
	}
	if length > 1048576 { //1024*1024=1048576
		t.Close()
		return 0, nil, fmt.Errorf(fmt.Sprintf("Read Error. Addr: %s; Err: too large data!", t.RemoteAddr().String()))
	}
	var lhead uint32
	buf := bytes.NewReader(data)
	binary.Read(buf, binary.LittleEndian, &lhead)

	tail := length - headerLen
	if lhead > 1048576 {
		t.Close()
		return 0, nil, fmt.Errorf(fmt.Sprintf("Read Error. Addr: %s; Err: too large data!", t.RemoteAddr().String()))
	}
	if uint32(tail) < lhead {
		return 0, nil, nil
	}
	adv = headerLen + int(lhead)
	token = data[:adv]
	return adv, token, nil
}

// start to extract payload
// stop when TCPConn is closed or net.TCPConn is closed
func (t *TCPConn) Scan() {
	defer t.CloseOnce()
	scanner := bufio.NewScanner(t)
	scanner.Split(t.split)

Circle:
	for scanner.Scan() {
		select {
		case <-t.Context.Done():
			t.info <- fmt.Sprintf("Conn Done. Addr: %s", t.RemoteAddr().String())
			break Circle
		default:
		}

		data := scanner.Bytes()
		msg := make([]byte, len(data))
		copy(msg, data)
		t.data <- msg
	}
	if err := scanner.Err(); err != nil {
		t.error <- err
	}
}

// Get data without four-bytes header
func (t *TCPConn) ReadData() []byte {
	data := <-t.data
	return data[headerLen:]
}

// Get string data
func (t *TCPConn) ReadString() string {
	data := <-t.data
	return string(data[headerLen:])
}

// Get data chan
func (t *TCPConn) GetDataChan() <-chan []byte {
	return t.data
}

// Get info chan
func (t *TCPConn) GetInfoChan() <-chan string {
	return t.info
}

// Get error chan
func (t *TCPConn) GetErrChan() <-chan error {
	return t.error
}

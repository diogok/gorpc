package gorpc

import (
	"bufio"
	"encoding/gob"
	"io"
	"net"
	"sync"
	"time"
)

type Client struct {
	Addr  string
	Conns int

	requestsChan chan *clientMessage
}

func (c *Client) Start() {
	c.requestsChan = make(chan *clientMessage, 1024)
	if c.Conns == 0 {
		c.Conns = 1
	}
	for i := 0; i < c.Conns; i++ {
		go clientHandler(c)
	}
}

func (c *Client) Send(request interface{}) interface{} {
	m := clientMessage{
		Request: request,
		Done:    make(chan struct{}, 1),
	}
	select {
	case c.requestsChan <- &m:
		<-m.Done
		return m.Response
	default:
		logError("rpc.Client: [%s]. Requests' queue with size=%d is overflown", cap(c.requestsChan), c.Addr)
		return nil
	}
}

func clientHandler(c *Client) {
	for {
		conn, err := net.Dial("tcp", c.Addr)
		if err != nil {
			logError("rpc.Client: [%s]. Cannot establish rpc connection: [%s]", c.Addr, err)
			time.Sleep(time.Second)
			continue
		}
		clientHandleConnection(c, conn)
	}
}

func clientHandleConnection(c *Client, conn net.Conn) {
	stopChan := make(chan struct{})

	pendingRequests := make(map[uint64]*clientMessage)
	var pendingRequestsLock sync.Mutex

	writerDone := make(chan struct{}, 1)
	go clientWriter(c, conn, pendingRequests, &pendingRequestsLock, stopChan, writerDone)

	readerDone := make(chan struct{}, 1)
	go clientReader(c, conn, pendingRequests, &pendingRequestsLock, readerDone)

	select {
	case <-writerDone:
		close(stopChan)
		conn.Close()
		<-readerDone
	case <-readerDone:
		close(stopChan)
		conn.Close()
		<-writerDone
	}

	for _, m := range pendingRequests {
		m.Done <- struct{}{}
	}
}

type clientMessage struct {
	Request  interface{}
	Response interface{}
	Done     chan struct{}
}

func clientWriter(c *Client, w io.Writer, pendingRequests map[uint64]*clientMessage, pendingRequestsLock *sync.Mutex, stopChan <-chan struct{}, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()

	var msgID uint64
	bw := bufio.NewWriter(w)
	e := gob.NewEncoder(bw)
	for {
		var rpcM *clientMessage

		msgID++
		select {
		case <-stopChan:
			return
		case rpcM = <-c.requestsChan:
		case <-time.After(10 * time.Millisecond):
			if err := bw.Flush(); err != nil {
				logError("rpc.Client: [%s]. Cannot flush requests to wire: [%s]", c.Addr, err)
				rpcM.Done <- struct{}{}
				return
			}
			select {
			case <-stopChan:
				return
			case rpcM = <-c.requestsChan:
			}
		}

		pendingRequestsLock.Lock()
		pendingRequests[msgID] = rpcM
		pendingRequestsLock.Unlock()

		m := wireMessage{
			ID:   msgID,
			Data: rpcM.Request,
		}
		if err := e.Encode(&m); err != nil {
			logError("rpc.Client: [%s]. Cannot send request to wire: [%s]", c.Addr, err)
			rpcM.Done <- struct{}{}
			pendingRequestsLock.Lock()
			delete(pendingRequests, msgID)
			pendingRequestsLock.Unlock()
			return
		}
	}
}

func clientReader(c *Client, r io.Reader, pendingRequests map[uint64]*clientMessage, pendingRequestsLock *sync.Mutex, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()

	br := bufio.NewReader(r)
	d := gob.NewDecoder(br)
	for {
		var m wireMessage
		if err := d.Decode(&m); err != nil {
			logError("rpc.Client: [%s]. Cannot read response from wire: [%s]", c.Addr, err)
			return
		}

		pendingRequestsLock.Lock()
		rpcM, ok := pendingRequests[m.ID]
		delete(pendingRequests, m.ID)
		pendingRequestsLock.Unlock()
		if !ok {
			logError("rpc.Client: [%s]. Unexpected msgID=[%d] obtained from server", c.Addr, m.ID)
			return
		}

		rpcM.Response = m.Data
		rpcM.Done <- struct{}{}
	}
}

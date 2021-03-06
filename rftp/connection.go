package rftp

import (
	"encoding"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

type packet struct {
	os         []option
	data       []byte
	ackNum     uint8
	remoteAddr *net.UDPAddr
}

type handlerFunc func(io.Writer, *packet)

func (h handlerFunc) handle(w io.Writer, p *packet) {
	h(w, p)
}

type packetHandler interface {
	handle(io.Writer, *packet)
}

type connection interface {
	addr() net.Addr
	handle(msgType uint8, h packetHandler)
	receive() error
	listen(host string) (func(), error)
	connectTo(host string) error
	send(msg encoding.BinaryMarshaler) error
	cclose(time.Duration) error
	LossSim(LossSimulator)
}

type udpConnection struct {
	lossSim    LossSimulator
	socket     *net.UDPConn
	handlers   map[uint8]packetHandler
	bufferSize int

	closed  chan struct{}
	closing bool
}

var _ connection = (*udpConnection)(nil)

type responseWriter func([]byte) (int, error)

func (rw responseWriter) Write(bs []byte) (int, error) {
	return rw(bs)
}

func NewUDPConnection() *udpConnection {
	return &udpConnection{
		lossSim:    &NoopLossSimulator{},
		handlers:   make(map[uint8]packetHandler),
		bufferSize: 2048,
		closed:     make(chan struct{}),
	}
}

func (c *udpConnection) addr() net.Addr {
	return c.socket.LocalAddr()
}

func (c *udpConnection) handle(msgType uint8, h packetHandler) {
	c.handlers[msgType] = h
}

func (c *udpConnection) cclose(deadline time.Duration) error {
	timeout := time.NewTimer(deadline)
	if c.closing {
		return fmt.Errorf("connection already closed")
	}
	c.closing = true
	err := c.socket.Close()
	log.Printf("closed connection with err: %v\n", err)
	select {
	case <-c.closed:
		log.Println("closed connection")
	case <-timeout.C:
		log.Println("timeout while closing connection")
	}
	return err
}

func (c *udpConnection) receive() error {
	var wg sync.WaitGroup

	for {
		msg := make([]byte, c.bufferSize)
		n, addr, err := c.socket.ReadFromUDP(msg)
		if err != nil {
			if c.closing {
				log.Println("finishing connection close")
				wg.Wait()
				c.closed <- struct{}{}
				log.Println("finished connection close")
				return nil
			}
			log.Printf("discarded packet due to error: %v", err)
			log.Println("closing due to crashed connection")
			return err
		}

		if c.lossSim.shouldDrop() {
			continue
		}

		header := &msgHeader{}
		if err := header.UnmarshalBinary(msg); err != nil {
			// Some wisdom: "Be conservative in what you do, be liberal in what you
			// accept from others."
			log.Printf("error while unmarshalling packet header: %v\n", err)
			continue
		}

		rw := responseWriter(func(bs []byte) (int, error) {
			return c.socket.WriteTo(bs, addr)
		})
		p := &packet{
			os:         header.options,
			data:       msg[header.hdrLen:n],
			remoteAddr: addr,
			ackNum:     header.ackNum,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if handler, ok := c.handlers[header.msgType]; !ok {
				log.Printf("no handler for message type %d\n", header.msgType)
			} else {
				handler.handle(rw, p)
			}
		}()
	}
}

func (c *udpConnection) listen(host string) (func(), error) {
	addr, err := net.ResolveUDPAddr("udp4", host)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, err
	}
	c.socket = conn

	return func() {
		conn.Close()
	}, nil
}

func (c *udpConnection) connectTo(host string) error {
	addr, err := net.ResolveUDPAddr("udp", host)
	if err != nil {
		return err
	}

	conn, err := net.DialUDP("udp", nil, addr)

	if err != nil {
		return err
	}

	c.socket = conn
	return nil
}

func (c udpConnection) send(msg encoding.BinaryMarshaler) error {
	return sendTo(c.socket, msg)
}

func (c *udpConnection) LossSim(lossSim LossSimulator) {
	c.lossSim = lossSim
}

func sendTo(writer io.Writer, msg encoding.BinaryMarshaler) error {
	header := msgHeader{
		version:   1,
		optionLen: 0,
	}

	switch v := msg.(type) {
	case clientRequest:
		header.msgType = msgClientRequest
	case clientAck:
		header.msgType = msgClientAck
		header.ackNum = v.ackNumber
	case serverMetaData:
		header.msgType = msgServerMetadata
	case serverPayload:
		log.Printf("sending payload: file %v at offset %v\n", v.fileIndex, v.offset)
		header.msgType = msgServerPayload
		header.ackNum = v.ackNumber
	case closeConnection:
		header.msgType = msgClose
	default:
		return fmt.Errorf("unknown msg type %T", v)
	}

	hs, err := header.MarshalBinary()
	if err != nil {
		return err
	}
	bs, err := msg.MarshalBinary()
	if err != nil {
		return err
	}

	_, err = writer.Write(append(hs, bs...))

	return err
}

var testConnectionAddr = &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 1000}

type testConnection struct {
	handlers map[uint8]packetHandler
	sentChan chan interface{} // sent out by application
	cancel   chan bool
	recvChan chan []byte // content is delivered to application, i.e., the test should fill this
}

var _ connection = (*testConnection)(nil)

func newTestConnection() *testConnection {
	return &testConnection{
		handlers: make(map[uint8]packetHandler),
		sentChan: make(chan interface{}, 100),
		cancel:   make(chan bool, 1),
		recvChan: make(chan []byte, 100),
	}
}

func (c *testConnection) addr() net.Addr {
	return nil
}

func (c *testConnection) handle(msgType uint8, h packetHandler) {
	c.handlers[msgType] = h
}

func (c *testConnection) receive() error {
	rw := responseWriter(func(bs []byte) (n int, err error) {
		n = len(bs)
		header := &msgHeader{}
		if err = header.UnmarshalBinary(bs); err != nil {
			// signal tests that this error occured?
			return n, nil
		}

		var msg encoding.BinaryUnmarshaler
		switch header.msgType {
		case msgClientRequest:
			msg = &clientRequest{}
		case msgServerMetadata:
			msg = &serverMetaData{}
		case msgServerPayload:
			msg = &serverPayload{}
		case msgClientAck:
			msg = &clientAck{}
		case msgClose:
			msg = &closeConnection{}
		default:
			return n, nil
		}

		if err = msg.UnmarshalBinary(bs); err != nil {
			return n, nil
		}

		c.sentChan <- msg
		return n, nil
	})

	for {
		select {
		case <-c.cancel:
			return nil
		case msg := <-c.recvChan:
			header := &msgHeader{}
			if err := header.UnmarshalBinary(msg); err != nil {
				return fmt.Errorf("error while unmarshalling packet header: %v", err)
			}

			p := &packet{
				os:         header.options,
				data:       msg[header.hdrLen:],
				remoteAddr: testConnectionAddr, // TODO: make configurable
			}
			go c.handlers[header.msgType].handle(rw, p)
		}
	}
}

func (c *testConnection) listen(host string) (func(), error) {
	return func() {
		c.cancel <- true
	}, nil
}

func (c testConnection) connectTo(host string) error {
	return nil
}

func (c testConnection) send(msg encoding.BinaryMarshaler) error {
	c.sentChan <- msg
	return nil
}

func (c testConnection) cclose(timeout time.Duration) error {
	return nil
}

func (c testConnection) LossSim(lossSim LossSimulator) {
}

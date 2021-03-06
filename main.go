// (c) 2017 - Bas Westerbaan <bas@westerbaan.name>
// You may redistribute this file under the conditions of the GPLv3.

// chaperoned is a simple single port TCP proxy with a twist --- the first
// incoming bytes are copied to a guardian which has to give its blessings
// before chaperoned will return the proxee's response.
//
//          +--------+        +------------+      +---------+
//          | client |  --->  | chaperoned | ---> | proxee  |
//          +--------+        +------------+      +---------+
//                                 |
//                                 V
//                             +----------+
//                             | guardian |
//                             +----------+
//
// chaperoned has several use cases.
//
// 1. It can be used to add Cookie-based authentication to a plain HTTP server.
// 2. ...
package main

// TODO splice!

import (
	"flag"
	"io"
	"log"
	"net"
)

type GuardianDecision int

const (
	GDError GuardianDecision = iota
	GDPassToProxee
	GDPassToGuardian
)

type WorkerFeedback int

const (
	WFFatalError           WorkerFeedback = iota // some fatal error occured in a worker
	WFDoNotWriteToGuardian                       // do not write to the guardian anymore
)

type Connection struct {
	id    int
	csock *net.TCPConn // socket to the client
	gsock *net.TCPConn // socket to the guardian
	psock *net.TCPConn // socket to the proxee

	feedback_chan          chan WorkerFeedback
	client_buffers_chan    chan []byte // buffers read from client
	guardian_decision_chan chan GuardianDecision

	write_to_guardian bool // copy client data to guardian
	write_to_proxee   bool // copy client data to proxee
}

var guardian_addr string
var proxee_addr string
var listen_addr string

var res_proxee_addr *net.TCPAddr
var res_guardian_addr *net.TCPAddr
var res_listen_addr *net.TCPAddr

var conns map[int]*Connection = make(map[int]*Connection)
var conn_closed_chan = make(chan int)

func main() {
	var err error

	// Parse cmdline flags
	flag.StringVar(&listen_addr, "listen", "localhost:8080",
		"Address to bind to, eg. localhost:8080")
	flag.StringVar(&proxee_addr, "proxee", "google.com:80",
		"TCP service to be proxeed, eg. google.com:80")
	flag.StringVar(&guardian_addr, "guardian", "localhost:4321",
		"Address of the guardian, eg. localhost:4321")
	flag.Parse()

	// Resolve addresses
	res_listen_addr, err = net.ResolveTCPAddr("tcp", listen_addr)
	if err != nil {
		log.Fatalf("Failed to resolve %v: %v", listen_addr, err)
	}
	res_guardian_addr, err = net.ResolveTCPAddr("tcp", guardian_addr)
	if err != nil {
		log.Fatalf("Failed to resolve %v: %v", guardian_addr, err)
	}
	res_proxee_addr, err = net.ResolveTCPAddr("tcp", proxee_addr)
	if err != nil {
		log.Fatalf("Failed to resolve %v: %v", proxee_addr, err)
	}

	// Set up listen socket
	lsock, err := net.ListenTCP("tcp", res_listen_addr)
	if err != nil {
		log.Fatalf("Failed to bind to %v: %v", listen_addr, err)
	}
	defer lsock.Close()

	csock_chan := make(chan *net.TCPConn)
	go RunAccepter(lsock, csock_chan)

	nconns := 0
	for {
		select {
		case csock := <-csock_chan:
			conn := &Connection{
				id:                nconns,
				csock:             csock,
				write_to_guardian: true,
				write_to_proxee:   true}
			conns[nconns] = conn
			go conn.Handle()
			nconns++
		case id := <-conn_closed_chan:
			delete(conns, id)
			log.Printf("%v: Handle returned", id)
		}
	}
}

// Accepts incoming connections and passes them back through a channel
func RunAccepter(lsock *net.TCPListener, schan chan<- *net.TCPConn) {
	for {
		csock, err := lsock.AcceptTCP()
		if err != nil {
			log.Printf("Error accepting: %v", err)
			continue
		}
		schan <- csock
	}
}

// Reads buffers from the client to a channel, from which its written
// to the proxee (and possibly guardian)
func (c *Connection) RunClientReader() {
	defer log.Printf("%v: RunClientReader returned", c.id)
	defer close(c.client_buffers_chan)

	for {
		buffer := make([]byte, 2048, 2048)
		nread, err := c.csock.Read(buffer)

		if nread == 0 {
			log.Printf("%v: No bytes read: %v", c.id, err)
			c.feedback_chan <- WFFatalError
			return
		}

		c.client_buffers_chan <- buffer[:nread]

		if err != nil {
			log.Printf("%v: Failed to read from client: %v", c.id, err)
			c.feedback_chan <- WFFatalError
			return
		}
	}
}

// Reads the first byte send by the guardian which contains the
// decision whether to connect the client to the proxee or guardian.
func (c *Connection) RunGuardianDecisionReader() {
	defer log.Printf("%v: RunGuardianDecisionReader returned", c.id)
	buffer := make([]byte, 1, 1)
	nread, err := c.gsock.Read(buffer)
	if nread == 0 {
		log.Printf("%v: Guardian closed connection: %v", c.id, err)
		c.guardian_decision_chan <- GDError
		return
	}
	switch buffer[0] {
	case 103: // g
		c.guardian_decision_chan <- GDPassToGuardian
	case 112: // p
		c.guardian_decision_chan <- GDPassToProxee
	default:
		log.Printf("%v: Guardian gave unrecognized decision: %v", c.id, buffer[0])
		c.guardian_decision_chan <- GDError
	}
}

// Pump data from the proxee to the client
func (c *Connection) RunProxeeToClientPump() {
	defer log.Printf("%v: RunProxeeToClientPump returned", c.id)
	log.Printf("%v: passing to proxee", c.id)
	_, err := io.Copy(c.csock, c.psock)
	if err == nil {
		c.csock.CloseWrite()
		return
	}
	log.Printf("%v: failed to pump from proxee to client: %v", c.id, err)
	c.feedback_chan <- WFFatalError
}

// Pump data from the guardian to the client
func (c *Connection) RunGuardianToClientPump() {
	defer log.Printf("%v: RunGuardianToClientPump returned", c.id)
	log.Printf("%v: passing to guardian", c.id)
	_, err := io.Copy(c.csock, c.gsock)
	if err == nil {
		c.csock.CloseWrite()
		return
	}
	log.Printf("%v: failed to pump from guardian to client: %v", c.id, err)
	c.feedback_chan <- WFFatalError
}

// Write data read from client to guardian and proxee
func (c *Connection) RunWriter() {
	defer log.Printf("%v: RunWriter returned", c.id)
	for c.write_to_guardian || c.write_to_proxee {
		buffer, not_closed := <-c.client_buffers_chan
		if !not_closed {
			break
		}

		offset := 0
		for c.write_to_guardian && offset != len(buffer) {
			nwritten, err := c.gsock.Write(buffer[offset:len(buffer)])
			if err != nil {
				log.Printf("%v: Failed to write to guardian: %v", c.id, err)
				c.write_to_guardian = false
				c.feedback_chan <- WFDoNotWriteToGuardian
				break
			}
			offset += nwritten
		}

		offset = 0
		for c.write_to_proxee && offset != len(buffer) {
			nwritten, err := c.psock.Write(buffer[offset:len(buffer)])
			if err != nil {
				log.Printf("%v: Failed to write to proxee: %v", c.id, err)
				return
			}
			offset += nwritten
		}
	}
}

func (c *Connection) Handle() {
	// First, connect to proxee and guardian
	defer func() { conn_closed_chan <- c.id }()
	defer c.csock.Close()

	log.Printf("%v: New connection from %v", c.id, c.csock.RemoteAddr())
	log.Printf("%v: Connecting to proxee", c.id)
	psock, err := net.DialTCP("tcp", nil, res_proxee_addr)
	if err != nil {
		log.Printf("%v: Failed to connect to proxee: %v", c.id, err)
		return
	}
	c.psock = psock
	defer c.psock.Close()

	gsock, err := net.DialTCP("tcp", nil, res_guardian_addr)
	if err != nil {
		log.Printf("%v: Failed to connect to guardian: %v", c.id, err)
		return
	}
	c.gsock = gsock
	defer c.gsock.Close()

	// Set up channels

	// NOTE there can be at most five feedback messages send to the feedback_chan
	c.feedback_chan = make(chan WorkerFeedback, 5)
	c.guardian_decision_chan = make(chan GuardianDecision)
	c.client_buffers_chan = make(chan []byte)

	// Start the workers

	go c.RunClientReader()
	go c.RunGuardianDecisionReader()
	go c.RunWriter()

	for {
		select {
		case what := <-c.guardian_decision_chan:
			switch what {
			case GDError:
				return
			case GDPassToProxee:
				c.write_to_guardian = false
				c.gsock.Close()
				go c.RunProxeeToClientPump()
			case GDPassToGuardian:
				c.write_to_proxee = false
				c.psock.Close()
				go c.RunGuardianToClientPump()
			}

		case what := <-c.feedback_chan:
			switch what {
			case WFFatalError:
				return
			case WFDoNotWriteToGuardian:
				c.write_to_guardian = false
			}
		}
		if !c.write_to_proxee && !c.write_to_guardian {
			return
		}
	}
}

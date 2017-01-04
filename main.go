// TODO copyright-line

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
// 1. It can be used to add Cookie-based authentication to a plain HTTP
//    server.
//          ... TODO
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

type Connection struct {
	id    int
	csock *net.TCPConn
	gsock *net.TCPConn
	psock *net.TCPConn

	errors_chan         chan bool
	client_buffers_chan chan []byte

	guardian_decision_chan chan GuardianDecision
}

var guardian_addr string
var proxee_addr string
var listen_addr string

var res_proxee_addr *net.TCPAddr
var res_guardian_addr *net.TCPAddr
var res_listen_addr *net.TCPAddr

var conns map[int]*Connection = make(map[int]*Connection)

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
	l, err := net.ListenTCP("tcp", res_listen_addr)
	if err != nil {
		log.Fatalf("Failed to bind to %v: %v", listen_addr, err)
	}
	defer l.Close()

	// Accept connection
	nconns := 0
	for {
		csock, err := l.AcceptTCP()
		if err != nil {
			log.Printf("Error accepting: %v", err)
			continue
		}

		conn := &Connection{
			id:    nconns,
			csock: csock}
		conns[nconns] = conn
		go conn.Handle()
		nconns++
	}
}

// Reads buffers from the client to a channel, from which its written
// to the proxee (and possibly guardian)
func (c *Connection) RunClientReader() {
	defer log.Printf("%v: RunClientReader returned", c.id)

	for {
		buffer := make([]byte, 2048, 2048)
		nread, err := c.csock.Read(buffer)

		if nread == 0 {
			log.Printf("%v: No bytes read: %v", c.id, err)
			c.errors_chan <- true
			return
		}

		c.client_buffers_chan <- buffer[:nread]

		if err != nil {
			log.Printf("%v: Failed to read from client: %v", c.id, err)
			c.errors_chan <- true
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
	case 103:
		c.guardian_decision_chan <- GDPassToGuardian
	case 112:
		c.guardian_decision_chan <- GDPassToProxee
	default:
		log.Printf("%v: Guardian gave unrecognized decision: %v", buffer[0])
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
	c.errors_chan <- true
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
	c.errors_chan <- true
}

func (c *Connection) Handle() {
	// First, connect to proxee and guardian
	defer log.Printf("%v: Handle returned", c.id)
	defer delete(conns, c.id)
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

	// NOTE there can be at most three errors send to the errors_chan
	c.errors_chan = make(chan bool, 3)
	c.client_buffers_chan = make(chan []byte)
	c.guardian_decision_chan = make(chan GuardianDecision)

	// Start the workers

	go c.RunClientReader()
	go c.RunGuardianDecisionReader()

	write_to_guardian := true
	write_to_proxee := true
	for {
		select {
		case what := <-c.guardian_decision_chan:
			switch what {
			case GDError:
				return
			case GDPassToProxee:
				write_to_guardian = false
				go c.RunProxeeToClientPump()
			case GDPassToGuardian:
				write_to_proxee = false
				go c.RunGuardianToClientPump()
			}
		case buffer := <-c.client_buffers_chan:
			offset := 0
			// TODO make this async, so we notice an error on the errors_chan
			//      earlier.
			for write_to_guardian && offset != len(buffer) {
				nwritten, err := c.gsock.Write(buffer[offset:len(buffer)])
				if err != nil {
					log.Printf("%v: Failed to write to guardian: %v", c.id, err)
					write_to_guardian = false
					break
				}
				// TODO nwritten can be 0?
				offset += nwritten
			}

			offset = 0
			for write_to_proxee && offset != len(buffer) {
				nwritten, err := c.psock.Write(buffer[offset:len(buffer)])
				if err != nil {
					log.Printf("%v: Failed to write to proxee: %v", c.id, err)
					return
				}
				// TODO nwritten can be 0?
				offset += nwritten
			}

			if !write_to_guardian && !write_to_proxee {
				return
			}
		case _ = <-c.errors_chan:
			return
		}
	}
}

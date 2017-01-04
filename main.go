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

import (
	"flag"
	"log"
	"net"
)

type Connection struct {
	id    int
	csock net.Conn
	gsock net.Conn
	psock net.Conn

	errors_chan         chan bool
	client_buffers_chan chan []byte

	// TODO make type
	// 0: error
	// 1: pass to proxee
	// 2: pass to guardian
	guardian_decision_chan chan int
}

var guardian_addr string
var proxee_addr string
var listen_addr string

var conns map[int]*Connection = make(map[int]*Connection)

func main() {
	// Parse cmdline flags
	flag.StringVar(&listen_addr, "listen", "localhost:8080",
		"Address to bind to, eg. localhost:8080")
	flag.StringVar(&proxee_addr, "proxee", "google.com:80",
		"TCP service to be proxeed, eg. google.com:80")
	flag.StringVar(&guardian_addr, "guardian", "localhost:4321",
		"Address of the guardian, eg. localhost:4321")
	flag.Parse()

	// Set up listen socket
	l, err := net.Listen("tcp", listen_addr)
	if err != nil {
		log.Fatalf("Failed to %v: %v", listen_addr, err)
	}
	defer l.Close()

	// Accept connection
	nconns := 0
	for {
		csock, err := l.Accept()
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

func (c *Connection) RunClientReader() {
	defer log.Printf("%v: RunClientReader returned", c.id)
	var buffer []byte = make([]byte, 2048, 2048)

	for {
		nread, err := c.csock.Read(buffer)
		if err != nil {
			log.Printf("%v: Failed to read from client: %v", c.id, err)
			c.errors_chan <- true
			return
		}

		if nread == 0 {
			log.Printf("%v: Client closed connection", c.id)
			c.errors_chan <- true
			return
		}

		c.client_buffers_chan <- buffer[:nread]
		// TODO should we make a copy of buffer or is this fine?
	}
}

func (c *Connection) RunGuardianDecisionReader() {
	defer log.Printf("%v: RunGuardianDecisionReader returned", c.id)
	buffer := make([]byte, 1, 1)
	nread, err := c.gsock.Read(buffer)
	if err != nil {
		log.Printf("%v: Failed to read from guardian: %v", c.id, err)
		c.guardian_decision_chan <- 0
		return
	}
	if nread == 0 {
		log.Printf("%v: Guardian closed connection", c.id)
		c.guardian_decision_chan <- 0
		return
	}
	if buffer[0] == 103 {
		c.guardian_decision_chan <- 2
	}
	if buffer[0] == 112 {
		c.guardian_decision_chan <- 1
	}
}

func (c *Connection) RunProxeeToClientPump() {
	defer log.Printf("%v: RunProxeeToClientPump returned", c.id)
	log.Printf("%v: passing to proxee", c.id)
	buffer := make([]byte, 2048, 2048)
	for {
		nread, err := c.psock.Read(buffer)
		if err != nil {
			log.Printf("%v: Failed to read from proxee: %v", c.id, err)
			c.errors_chan <- true
			return
		}

		if nread == 0 {
			log.Printf("%v: Proxee closed connection", c.id)
			c.errors_chan <- true
			return
		}

		offset := 0
		for offset != nread {
			nwritten, err := c.csock.Write(buffer[offset:nread])
			if err != nil {
				log.Printf("%v: Failed to write to client: %v", c.id, err)
				c.errors_chan <- true
				return
			}
			// TODO nwritten == 0
			offset += nwritten
		}
	}
}

func (c *Connection) RunGuardianToClientPump() {
	defer log.Printf("%v: RunGuardianToClientPump returned", c.id)
	log.Printf("%v: passing to guardian", c.id)
	buffer := make([]byte, 2048, 2048)
	for {
		nread, err := c.gsock.Read(buffer)
		if err != nil {
			log.Printf("%v: Failed to read from guardian: %v", c.id, err)
			c.errors_chan <- true
			return
		}

		if nread == 0 {
			log.Printf("%v: Guardian closed connection", c.id)
			c.errors_chan <- true
			return
		}

		offset := 0
		for offset != nread {
			nwritten, err := c.csock.Write(buffer[offset:nread])
			if err != nil {
				log.Printf("%v: Failed to write to client: %v", c.id, err)
				c.errors_chan <- true
				return
			}
			// TODO nwritten == 0
			offset += nwritten
		}
	}
}

func (c *Connection) Handle() {
	// First, connect to proxee and guardian
	defer log.Printf("%v: Handle returned", c.id)
	defer delete(conns, c.id)
	defer c.csock.Close()

	log.Printf("%v: New connection from %v", c.id, c.csock.RemoteAddr())
	log.Printf("%v: Connecting to proxee", c.id)
	psock, err := net.Dial("tcp", proxee_addr)
	if err != nil {
		log.Printf("%v: Failed to connect to proxee: %v", c.id, err)
		return
	}
	c.psock = psock
	defer c.psock.Close()

	gsock, err := net.Dial("tcp", guardian_addr)
	if err != nil {
		log.Printf("%v: Failed to connect to guardian: %v", c.id, err)
		return
	}
	c.gsock = gsock
	defer c.gsock.Close()

	// Set up channels
	c.errors_chan = make(chan bool)
	c.client_buffers_chan = make(chan []byte)
	c.guardian_decision_chan = make(chan int)

	// Start the workers

	go c.RunClientReader()
	go c.RunGuardianDecisionReader()

	guardian_write_ok := true
	for {
		select {
		case what := <-c.guardian_decision_chan:
			switch what {
			case 0: // error
				return
			case 1: // pass to proxee
				go c.RunProxeeToClientPump()
			case 2: // pass to guardian
				go c.RunGuardianToClientPump()
			}
		case buffer := <-c.client_buffers_chan:
			offset := 0
			for offset != len(buffer) && guardian_write_ok {
				nwritten, err := c.gsock.Write(buffer[offset:len(buffer)])
				if err != nil {
					log.Printf("%v: Failed to write to guardian: %v", c.id, err)
					guardian_write_ok = false
					break
				}
				// TODO nwritten can be 0?
				offset += nwritten
			}

			offset = 0
			for offset != len(buffer) {
				nwritten, err := c.psock.Write(buffer[offset:len(buffer)])
				if err != nil {
					log.Printf("%v: Failed to write to proxee: %v", c.id, err)
					return
				}
				// TODO nwritten can be 0?
				offset += nwritten
			}
		case _ = <-c.errors_chan:
		default:
			// ..
		}

	}
}

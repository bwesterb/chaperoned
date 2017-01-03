package main

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
//  1. It can be used to add Cookie-based authentication to a plain HTTP
//     server.
//          ... TODO

import (
	"log"
	"net"
)

var guardian string = "localhost:4321"
var proxee string = "google.com:80"
var bind_to string = "localhost:8080"

func main() {

	l, err := net.Listen("tcp", bind_to)
	if err != nil {
		log.Fatalf("Failed to %v: %v", bind_to, err)
	}
	defer l.Close()

	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("Error accepting: %v", err)
			continue
		}

		go handleRequest(conn)
	}

}

func handleRequest(cconn net.Conn) {
	defer cconn.Close()

	log.Printf("Connecting to proxee for: %v", cconn.RemoteAddr())
	pconn, err := net.Dial("tcp", proxee)
	if err != nil {
		log.Printf("Failed to connect to proxee: %v", err)
		return
	}
	defer pconn.Close()

	gconn, err := net.Dial("tcp", guardian)
	if err != nil {
		log.Printf("Failed to connect to guardian: %v", err)
		return
	}
	defer gconn.Close()

	// 0: error
	// 1: pass to proxee
	// 2: pass to guardian
	guardian_reader_chan := make(chan int)
	guardian_reader_error_chan := make(chan bool)
	client_reader_error_chan := make(chan bool)
	client_reader_buffer_chan := make(chan []byte)
	proxee_reader_error_chan := make(chan bool)

	go func() {
		var buffer []byte = make([]byte, 2048, 2048)

		for {
			nread, err := cconn.Read(buffer)
			if err != nil {
				log.Printf("Failed to read from client: %v", err)
				client_reader_error_chan <- true
				return
			}

			if nread == 0 {
				log.Printf("Client closed connection")
				client_reader_error_chan <- false
				return
			}

			client_reader_buffer_chan <- buffer[:nread]
			// TODO should we make a copy of buffer or is this fine?
		}
	}()

	// Checking guardian response
	go func() {
		buffer := make([]byte, 1, 1)
		nread, err := gconn.Read(buffer)
		if err != nil {
			log.Printf("Failed to read from guardian: %v", err)
			guardian_reader_chan <- 0
			return
		}
		if nread == 0 {
			log.Printf("Guardian closed connection")
			guardian_reader_chan <- 0
			return
		}
		if buffer[0] == 103 {
			guardian_reader_chan <- 2
		}
		if buffer[0] == 112 {
			guardian_reader_chan <- 1
		}
	}()

	guardian_write_ok := true
	for {
		select {
		case what := <-guardian_reader_chan:
			switch what {
			case 0: // error
				return
			case 1: // pass to proxee
				log.Printf("%v: passing to proxee", cconn.RemoteAddr())
				go func() {
					buffer := make([]byte, 2048, 2048)
					for {
						nread, err := pconn.Read(buffer)
						if err != nil {
							log.Printf("Failed to read from proxee: %v", err)
							proxee_reader_error_chan <- true
							return
						}

						if nread == 0 {
							log.Printf("Proxee closed connection")
							proxee_reader_error_chan <- false
							return
						}

						offset := 0
						for offset != nread {
							nwritten, err := cconn.Write(buffer[offset:nread])
							if err != nil {
								log.Printf("Failed to write to client: %v", err)
								proxee_reader_error_chan <- true
								return
							}
							// TODO nwritten == 0
							offset += nwritten
						}
					}
				}()
			case 2: // pass to guardian
				log.Printf("%v: passing to guardian", cconn.RemoteAddr())
				go func() {
					buffer := make([]byte, 2048, 2048)
					for {
						nread, err := gconn.Read(buffer)
						if err != nil {
							log.Printf("Failed to read from guardian: %v", err)
							guardian_reader_error_chan <- true
							return
						}

						if nread == 0 {
							log.Printf("Guardian closed connection")
							guardian_reader_error_chan <- false
							return
						}

						offset := 0
						for offset != nread {
							nwritten, err := cconn.Write(buffer[offset:nread])
							if err != nil {
								log.Printf("Failed to write to client: %v", err)
								guardian_reader_error_chan <- true
								return
							}
							// TODO nwritten == 0
							offset += nwritten
						}
					}
				}()
			}
		case buffer := <-client_reader_buffer_chan:
			offset := 0
			for offset != len(buffer) && guardian_write_ok {
				nwritten, err := gconn.Write(buffer[offset:len(buffer)])
				if err != nil {
					log.Printf("Failed to write to guardian: %v", err)
					guardian_write_ok = false
					break
				}
				// TODO nwritten can be 0?
				offset += nwritten
			}

			offset = 0
			for offset != len(buffer) {
				nwritten, err := pconn.Write(buffer[offset:len(buffer)])
				if err != nil {
					log.Printf("Failed to write to proxee: %v", err)
					return
				}
				// TODO nwritten can be 0?
				offset += nwritten
			}
		case _ = <-client_reader_error_chan:
			return
		case _ = <-proxee_reader_error_chan:
			return
		case _ = <-guardian_reader_error_chan:
			return
		default:
			// ..
		}

	}
}

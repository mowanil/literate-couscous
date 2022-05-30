package main

import (
	"bytes"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

var (
	newline = []byte{'\n'}
	space   = []byte{' '}
)

// Client stays in between websocket connection and Hub
type Client struct {
	hub *Hub
	// Websocket connection
	conn *websocket.Conn
	// Buffered channel of outbound messages
	send chan []byte
}

// readPump pumps messages from the websocket connection to the Hub
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			break
		}
		message = bytes.TrimSpace(bytes.Replace(message, newline, space, -1))
		c.hub.broadcast <- message
	}
}

// writePump pumps messages from the Hub to the websockets connections
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Add queued messages to the current websocket message
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write(newline)
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// Hub mantains the set of active clients and broadcasts messages to the clients
type Hub struct {
	// Registed connected clients
	clients map[*Client]bool

	// Inbound messages from the clients
	broadcast chan []byte

	// Register request from the clients
	register chan *Client

	// Unregister requests from the clients
	unregister chan *Client
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
		case message := <-h.broadcast:
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
		}
	}
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

var muxDispatcher = mux.NewRouter()

type Router interface {
	GET(uri string, f func(http.ResponseWriter, *http.Request))
	SERVE(port string)
}

type muxRouter struct{}

func NewMuxRouter() Router {
	return &muxRouter{}
}

func (*muxRouter) GET(uri string, f func(http.ResponseWriter, *http.Request)) {
	muxDispatcher.HandleFunc(uri, f).Methods(http.MethodGet)
}

func (*muxRouter) SERVE(port string) {
	log.Println("Up and running at: http://localhost:8081")
	http.ListenAndServe(port, muxDispatcher)
}

func main() {
	router := NewMuxRouter()
	router.GET("/ws", func(rw http.ResponseWriter, r *http.Request) {
		hub := newHub()
		go hub.run()

		// we will run client application from another address
		// so request header always acceptable
		upgrader.CheckOrigin = func(r *http.Request) bool { return true }

		// upgrade http server connection to websocket protocol
		c, err := upgrader.Upgrade(rw, r, nil)
		if err != nil {
			log.Print("Upgrade:", err)
			return
		}

		// create client that references to hub
		client := &Client{hub: hub, conn: c, send: make(chan []byte, 1024)}
		// it feels weird at first look
		client.hub.register <- client

		go client.writePump()
		go client.readPump()
	})

	router.SERVE(":8081")
}

package mes

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket" // Asumimos el estándar de facto, seguro y testeado en batalla
)

const (
	writeWait      = 2 * time.Second
	pongWait       = 10 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 1024
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	// En planta, el CORS debe ser restrictivo, pero para el Hub permitimos conexiones internas
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Client representa un HMI conectado.
type Client struct {
	hub  *Hub
	conn *websocket.Conn
	send chan []byte
}

// Hub orquesta las conexiones y la serialización única.
type Hub struct {
	dispatcher *EventDispatcher
	clients    map[*Client]bool
	register   chan *Client
	unregister chan *Client
	mu         sync.Mutex
	done       chan struct{} // closed on shutdown, signals writePump to exit
}

func NewHub(dispatcher *EventDispatcher) *Hub {
	return &Hub{
		dispatcher: dispatcher,
		clients:    make(map[*Client]bool),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		done:       make(chan struct{}),
	}
}

// Start levanta el motor de multiplexación y escucha al dispatcher.
func (h *Hub) Start(ctx context.Context) {
	log.Println("[SRE] HMI WebSocket Hub inicializado")
	for {
		select {
		case <-ctx.Done():
			h.shutdown()
			return
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
		case client := <-h.unregister:
			h.removeClient(client)
		case event := <-h.dispatcher.wsHubCh:
			// SINGLE-MARSHAL: CPU optimizado, se serializa 1 vez, se envía N veces.
			payload, err := json.Marshal(event)
			if err != nil {
				log.Printf("[ERROR] Hub incapaz de serializar evento: %v", err)
				continue
			}

			h.mu.Lock()
			for client := range h.clients {
				select {
				case client.send <- payload:
				default:
					// SRE FAIL-FAST: El cliente no lee lo suficientemente rápido.
					// Cortamos el circuito para este cliente específico sin afectar a los demás.
					h.removeClientLocked(client)
				}
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) removeClient(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.removeClientLocked(client)
}

func (h *Hub) removeClientLocked(client *Client) {
	if _, ok := h.clients[client]; ok {
		delete(h.clients, client)
		go func() {
			close(client.send)
			_ = client.conn.Close() // I/O desvinculado del Mutex
		}()
	}
}

func (h *Hub) shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.done: // already closed
	default:
		close(h.done)
	}
	for client := range h.clients {
		_ = client.conn.Close()
		close(client.send)
		delete(h.clients, client)
	}
}

// writePump empuja los datos del canal interno del cliente hacia el socket TCP.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.hub.unregister <- c
	}()

	for {
		select {
		case <-c.hub.done:
			return
		case message, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ServeWS es el handler HTTP que intercepta el upgrade a WebSocket.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[SRE] Error en WS Upgrade: %v", err)
		return
	}

	// Buffer de 256 frames. Si se llena, el select por defecto lo expulsa.
	client := &Client{hub: h, conn: conn, send: make(chan []byte, 256)}
	client.hub.register <- client

	// Iniciamos el bombeo de escritura hacia el socket
	go client.writePump()

	// El hilo principal de ServeWS funciona como readPump (necesario para procesar pongs)
	client.conn.SetReadLimit(maxMessageSize)
	_ = client.conn.SetReadDeadline(time.Now().Add(pongWait))
	client.conn.SetPongHandler(func(string) error { _ = client.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })

	for {
		if _, _, err := client.conn.ReadMessage(); err != nil {
			break // El HMI se desconectó o falló el latido (Ping/Pong)
		}
	}
	client.hub.unregister <- client
}

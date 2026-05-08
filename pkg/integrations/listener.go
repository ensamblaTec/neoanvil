package integrations

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"net"
	"strings"
	"sync/atomic"
)

type SREListener struct {
	addr       string
	activeFlag atomic.Bool
	onAlert    func(payload string)
}

func NewSREListener(addr string, onAlertCallback func(payload string)) *SREListener {
	return &SREListener{
		addr:    addr,
		onAlert: onAlertCallback,
	}
}

func (l *SREListener) Start(ctx context.Context) {
	ln, err := net.Listen("tcp", l.addr)
	if err != nil {
		log.Printf("[SRE-FATAL] Listener TCP colapso: %v\n", err)
		return
	}
	l.activeFlag.Store(true)
	log.Printf("[SRE-LISTENER] Ouroboros Socket en %s\n", l.addr)

	go l.acceptLoop(ctx, ln)
}

func (l *SREListener) acceptLoop(ctx context.Context, ln net.Listener) {
	go func() { <-ctx.Done(); ln.Close() }() //nolint:errcheck
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // ctx cancelled → ln closed
		}
		go l.handleConn(conn)
	}
}

func (l *SREListener) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	scanner := bufio.NewScanner(conn)

	body := l.readBody(scanner)
	l.parseAndAlert(body)
}

func (l *SREListener) readBody(scanner *bufio.Scanner) string {
	var body strings.Builder
	inBody := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			inBody = true
			continue
		}
		if inBody {
			body.WriteString(line)
		}
	}
	return body.String()
}

func (l *SREListener) parseAndAlert(body string) {
	var payload struct {
		Text  string `json:"text"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&payload); err == nil {
		msg := "Alerta silenciosa"
		if payload.Text != "" {
			msg = payload.Text
		} else if payload.Title != "" {
			msg = payload.Title
		}
		l.onAlert(msg)
	}
}

func (l *SREListener) IsActive() bool {
	return l.activeFlag.Load()
}

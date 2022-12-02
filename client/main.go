package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"github.com/Pallinder/go-randomdata"
	"github.com/burntcarrot/rowix/crdt"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nsf/termbox-go"
	"log"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type message struct {
	Username  string        `json:"username"`
	Text      string        `json:"text"`
	Type      string        `json:"type"`
	ID        uuid.UUID     `json:"ID"`
	Operation Operation     `json:"operation"`
	Document  crdt.Document `json:"document"`
}

type Operation struct {
	Type     string `json:"type"`
	Position int    `json:"position"`
	Value    string `json:"value"`
}

// Local document containing content.
var doc crdt.Document

// Centralized logger.
var logger *log.Logger

// termbox-based editor.
var e *Editor

func main() {
	// Parse flags.
	server := flag.String("server", "localhost:8080", "Server network address")
	path := flag.String("path", "/", "Server path")
	secure := flag.Bool("wss", false, "Use wss by default")
	login := flag.Bool("login", false, "Whether the login process before joining is necessary")
	flag.Parse()

	// Construct WebSocket URL.
	var u url.URL
	if *secure {
		u = url.URL{Scheme: "wss", Host: *server, Path: *path}
	} else {
		u = url.URL{Scheme: "ws", Host: *server, Path: *path}
	}

	var name string
	var s *bufio.Scanner

	// Read username based if login flag is set to true, otherwise generate a random name.
	if *login {
		fmt.Print("Enter your name: ")
		s = bufio.NewScanner(os.Stdin)
		s.Scan()
		name = s.Text()
	} else {
		name = randomdata.SillyName()
	}

	// Initialize document.
	doc = crdt.New()

	// Get WebSocket connection.
	dialer := websocket.Dialer{
		HandshakeTimeout: 2 * time.Minute,
	}

	conn, _, err := dialer.Dial(u.String(), nil)
	if err != nil {
		fmt.Printf("Connection error, exiting: %s\n", err)
		os.Exit(0)
	}
	defer conn.Close()

	// Send joining message.
	msg := message{Username: name, Text: "has joined the session.", Type: "info"}
	_ = conn.WriteJSON(msg)

	// open logging file  and create if non-existent
	file, err := os.OpenFile("help.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Logger error, exiting: %s", err)
		os.Exit(0)
	}
	defer file.Close()

	logger = log.New(file, "--- name: "+name+" >> ", log.LstdFlags)

	logger = log.New(file, fmt.Sprintf("--- name: %s >> ", name), log.LstdFlags)
	// start local session
	err = UI(conn, &doc)
	if err != nil {
		if strings.HasPrefix(err.Error(), "rowix") {
			fmt.Println("exiting session.")
			os.Exit(0)
		}
		fmt.Printf("TUI error, exiting: %s\n", err)
		os.Exit(0)
	}
}

// UI creates a new editor view and runs the main loop.
func UI(conn *websocket.Conn, d *crdt.Document) error {
	err := termbox.Init()
	if err != nil {
		return err
	}
	defer termbox.Close()

	e = NewEditor()
	e.SetSize(termbox.Size())
	e.Draw()

	err = mainLoop(e, conn, d)
	if err != nil {
		return err
	}

	return nil
}

// mainLoop is the main update loop for the UI.
func mainLoop(e *Editor, conn *websocket.Conn, doc *crdt.Document) error {
	termboxChan := getTermboxChan()
	msgChan := getMsgChan(conn)

	// event select
	for {
		select {
		case termboxEvent := <-termboxChan:
			err := handleTermboxEvent(termboxEvent, conn)
			if err != nil {
				return err
			}

		case msg := <-msgChan:
			handleMsg(msg, doc, conn)
		}
	}
}

// handleTermboxEvent handles key input by updating the local CRDT document and sending a message over the WebSocket connection.
func handleTermboxEvent(ev termbox.Event, conn *websocket.Conn) error {
	switch ev.Type {
	case termbox.EventKey:
		switch ev.Key {
		case termbox.KeyEsc, termbox.KeyCtrlC:
			return errors.New("rowix: exiting")
		case termbox.KeyArrowLeft, termbox.KeyCtrlB:
			e.MoveCursor(-1, 0)
		case termbox.KeyArrowRight, termbox.KeyCtrlF:
			e.MoveCursor(1, 0)
		case termbox.KeyHome:
			e.SetX(0)
		case termbox.KeyEnd:
			e.SetX(len(e.text))
		case termbox.KeyBackspace, termbox.KeyBackspace2:
			performOperation(OperationDelete, ev, conn)
		case termbox.KeyDelete:
			performOperation(OperationDelete, ev, conn)
		case termbox.KeyTab: // TODO: add tabs?
		case termbox.KeyEnter:
			logger.Println("enter value:", ev.Ch)
			ev.Ch = '\n'
			performOperation(OperationInsert, ev, conn)
		case termbox.KeySpace:
			logger.Println("space value:", ev.Ch)
			ev.Ch = ' '
			performOperation(OperationInsert, ev, conn)
		default:
			if ev.Ch != 0 {
				performOperation(OperationInsert, ev, conn)
			}
		}
	}
	logger.Printf("TERMBOX: length of text: %v, cursor position: %v\n", len(e.text), e.cursor)
	e.Draw()
	return nil
}

const (
	OperationInsert = iota
	OperationDelete
)

// performOperation performs a CRDT insert or delete operation on the local document and sends a message over the WebSocket connection
func performOperation(opType int, ev termbox.Event, conn *websocket.Conn) {
	// Get position and value.
	ch := string(ev.Ch)

	var msg message

	// Modify local state (CRDT) first.
	switch opType {
	case OperationInsert:
		logger.Printf("LOCAL INSERT: %s at cursor position %v\n", ch, e.cursor)
		r := []rune(ch)
		e.AddRune(r[0])

		text, err := doc.Insert(e.cursor, ch)
		if err != nil {
			e.SetText(text)
			logger.Printf("CRDT error: %v\n", err)
		}

		e.SetText(text)
		msg = message{Type: "operation", Operation: Operation{Type: "insert", Position: e.cursor, Value: ch}}
	case OperationDelete:
		logger.Printf("LOCAL DELETE:  cursor position %v\n", e.cursor)
		if e.cursor-1 <= 0 {
			e.cursor = 1
		}
		text := doc.Delete(e.cursor)
		e.SetText(text)
		msg = message{Type: "operation", Operation: Operation{Type: "delete", Position: e.cursor}}
		e.MoveCursor(-1, 0)
	}
	// logger.Println(crdt.Content(doc))
	print(doc)
	_ = conn.WriteJSON(msg)
}

func print(doc crdt.Document) {
	logger.Printf("---DOCUMENT STATE---")
	for i, c := range doc.Characters {
		logger.Printf("index: %v  value: %s  ID: %v  IDPrev: %v  IDNext: %v  ", i, c.Value, c.ID, c.IDPrevious, c.IDNext)
	}
}

// getTermboxChan returns a channel of termbox Events repeatedly waiting on user input.
func getTermboxChan() chan termbox.Event {
	termboxChan := make(chan termbox.Event)
	go func() {
		for {
			termboxChan <- termbox.PollEvent()
		}
	}()
	return termboxChan
}

// handleMsg updates the CRDT document with the contents of the message.
func handleMsg(msg message, doc *crdt.Document, conn *websocket.Conn) {
	if msg.Type == "docResp" { //update local document
		logger.Printf("DOCRESP RECEIVED, updating local doc%+v\n", msg.Document)
		logger.Printf("MESSAGE DOC: %+v\n", msg.Document)
		// doc.SetText(msg.Document)
		// logger.Printf("COPIED DOC:  %+v\n{}", *doc)
		*doc = msg.Document
	} else if msg.Type == "docReq" { // send local document as docResp message
		logger.Printf("DOCREQ RECEIVED, sending local document to %v\n", msg.ID)
		docMsg := message{Type: "docResp", Document: *doc, ID: msg.ID}
		conn.WriteJSON(&docMsg)
	} else if msg.Type == "SiteID" {
		siteID, err := strconv.Atoi(msg.Text)
		if err != nil {
			panic(err)
		}
		crdt.SiteID = siteID
		logger.Printf("SITE ID %v, INTENDED SITE ID: %v", crdt.SiteID, siteID)
	} else {
		switch msg.Operation.Type {
		case "insert":
			_, _ = doc.Insert(msg.Operation.Position, msg.Operation.Value)
			logger.Printf("REMOTE INSERT: %s at position %v\n", msg.Operation.Value, msg.Operation.Position)
		case "delete":
			_ = doc.Delete(msg.Operation.Position)
			logger.Printf("REMOTE DELETE: position %v\n", msg.Operation.Position)
		}
	}
	// logger.Printf("cursor position: %v, document length: %v\n", )
	print(*doc)
	e.SetText(crdt.Content(*doc))
	// logger.Printf("MESSAGE: length of text: %v, cursor position: %v\n", len(e.text), e.cursor)
	e.Draw()
}

// getMsgChan returns a message channel that repeatedly reads from a websocket connection.
func getMsgChan(conn *websocket.Conn) chan message {
	messageChan := make(chan message)
	go func() {
		for {
			var msg message

			// Read message.
			err := conn.ReadJSON(&msg)
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("websocket error: %v", err)
				}
				break
			}

			logger.Printf("message received: %+v\n", msg)

			// send message through channel
			messageChan <- msg

		}
	}()
	return messageChan
}

// Package atmodem implements bounded, serialized AT exchanges. Transport loss or
// a timeout ends the session: a delayed OK must never acknowledge the next action.
package atmodem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type Error struct{ Result string }

func (e *Error) Error() string { return "AT: " + e.Result }

type Client struct {
	stream io.ReadWriteCloser
	gate   chan struct{}
	lines  chan string
	done   chan struct{}
	once   sync.Once
}

func New(stream io.ReadWriteCloser) *Client {
	c := &Client{stream: stream, gate: make(chan struct{}, 1), lines: make(chan string, 256), done: make(chan struct{})}
	go c.read()
	return c
}
func (c *Client) Close() error {
	c.once.Do(func() { close(c.done); _ = c.stream.Close() })
	return nil
}
func (c *Client) Alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

// Synchronize cancels a possible abandoned SMS prompt before any new session
// command. It only sends ESC/CR, then waits for a quiet interval. A stale OK
// from an older process can never authorize a new send or call.
func (c *Client) Synchronize(ctx context.Context) error {
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return io.EOF
	}
	defer func() { <-c.gate }()
	if _, err := c.stream.Write([]byte{0x1b, '\r'}); err != nil {
		c.Close()
		return err
	}
	quiet := time.NewTimer(250 * time.Millisecond)
	defer quiet.Stop()
	for {
		select {
		case <-ctx.Done():
			c.Close()
			return ctx.Err()
		case <-c.done:
			return io.EOF
		case <-c.lines:
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(250 * time.Millisecond)
		case <-quiet.C:
			return nil
		}
	}
}
func (c *Client) emit(s string) bool {
	select {
	case c.lines <- s:
		return true
	case <-c.done:
		return false
	default:
		c.Close()
		return false
	}
}
func (c *Client) read() {
	defer c.Close()
	buffer := make([]byte, 4096)
	line := make([]byte, 0, 256)
	for {
		n, err := c.stream.Read(buffer)
		for _, b := range buffer[:n] {
			if b == '\n' || b == '\r' {
				if s := strings.TrimSpace(string(line)); s != "" && !c.emit(s) {
					return
				}
				line = line[:0]
			} else if b == '>' && len(strings.TrimSpace(string(line))) == 0 {
				if !c.emit(">") {
					return
				}
				line = line[:0]
			} else {
				line = append(line, b)
				if len(line) > 16384 {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}
func terminal(line string) error {
	if line == "ERROR" || strings.HasPrefix(line, "+CME ERROR:") || strings.HasPrefix(line, "+CMS ERROR:") {
		return &Error{Result: line}
	}
	return nil
}

// Command sends exactly once. SMS payload is sent only after a prompt; failures
// are deliberately not retried because a network submission may have succeeded.
func (c *Client) Command(ctx context.Context, command string, payload []byte) ([]string, error) {
	if !strings.HasPrefix(command, "AT") || strings.ContainsAny(command, "\r\n\x1a\x1b") {
		return nil, errors.New("invalid AT command")
	}
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, io.EOF
	}
	defer func() { <-c.gate }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.Alive() {
		return nil, io.EOF
	}
	// Discard idle unsolicited lines. Call/message state is queried explicitly.
drain:
	for {
		select {
		case <-c.lines:
		default:
			break drain
		}
	}
	if _, err := io.WriteString(c.stream, command+"\r"); err != nil {
		c.Close()
		return nil, err
	}
	lines := []string{}
	bytes := 0
	submitted := payload == nil
	for {
		select {
		case <-ctx.Done():
			c.Close()
			return lines, ctx.Err()
		case <-c.done:
			return lines, io.EOF
		case line := <-c.lines:
			if line == command {
				continue
			}
			if err := terminal(line); err != nil {
				return lines, err
			}
			if line == "NO CARRIER" || line == "BUSY" || line == "NO ANSWER" {
				if strings.HasPrefix(command, "ATD") || command == "ATA" {
					return lines, &Error{Result: line}
				}
				continue
			}
			if line == ">" {
				if submitted {
					c.Close()
					return lines, errors.New("unexpected AT prompt")
				}
				submitted = true
				data := append(append([]byte{}, payload...), 0x1a)
				if _, err := c.stream.Write(data); err != nil {
					c.Close()
					return lines, err
				}
				continue
			}
			if line == "OK" {
				if !submitted {
					c.Close()
					return lines, errors.New("SMS prompt missing")
				}
				return lines, nil
			}
			bytes += len(line)
			if bytes > 128<<10 {
				c.Close()
				return nil, fmt.Errorf("AT response too large")
			}
			lines = append(lines, line)
		}
	}
}

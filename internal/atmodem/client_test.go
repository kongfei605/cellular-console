package atmodem

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestCommandPromptFragmentationAndUnsolicitedLines(t *testing.T) {
	a, b := net.Pipe()
	c := New(a)
	defer c.Close()
	defer b.Close()
	errors := make(chan error, 1)
	go func() {
		r := bufio.NewReader(b)
		s, e := r.ReadString('\r')
		if e != nil {
			errors <- e
			return
		}
		if s != "AT+CMGS=4\r" {
			errors <- io.ErrUnexpectedEOF
			return
		}
		for _, part := range []string{"\r\nRING\r", "\n>", " "} {
			if _, e = io.WriteString(b, part); e != nil {
				errors <- e
				return
			}
		}
		payload, e := r.ReadString(0x1a)
		if e != nil {
			errors <- e
			return
		}
		if payload != "ABCD\x1a" {
			errors <- io.ErrUnexpectedEOF
			return
		}
		_, e = io.WriteString(b, "\r\n+CMGS: 7\r\nOK\r\n")
		errors <- e
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lines, err := c.Command(ctx, "AT+CMGS=4", []byte("ABCD"))
	if err != nil || !strings.Contains(strings.Join(lines, "\n"), "+CMGS: 7") {
		t.Fatal(lines, err)
	}
	if err := <-errors; err != nil {
		t.Fatal(err)
	}
}
func TestTimeoutClosesSessionAndCannotAcknowledgeNextCommand(t *testing.T) {
	a, b := net.Pipe()
	c := New(a)
	defer c.Close()
	defer b.Close()
	go func() { buf := make([]byte, 100); _, _ = b.Read(buf) }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Command(ctx, "AT+CSQ", nil); err == nil {
		t.Fatal("timeout accepted")
	}
	if c.Alive() {
		t.Fatal("timed-out session still reusable")
	}
	if _, err := c.Command(context.Background(), "ATD12025550123;", nil); err == nil {
		t.Fatal("action accepted on stale session")
	}
}
func TestCommandRejectsInjectionAndPreservesSessionForModemErrors(t *testing.T) {
	a, b := net.Pipe()
	c := New(a)
	defer c.Close()
	defer b.Close()
	if _, err := c.Command(context.Background(), "AT\rATD12025550123;", nil); err == nil {
		t.Fatal("command injection")
	}
	go func() {
		r := bufio.NewReader(b)
		_, _ = r.ReadString('\r')
		_, _ = io.WriteString(b, "\r\n+CME ERROR: 516\r\n")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.Command(ctx, "AT+QGPSLOC=2", nil); err == nil || !c.Alive() {
		t.Fatal("modem error lost session", err)
	}
}

func TestCallURCDoesNotAbortUnrelatedCommand(t *testing.T) {
	a, b := net.Pipe()
	c := New(a)
	defer c.Close()
	defer b.Close()
	go func() {
		r := bufio.NewReader(b)
		_, _ = r.ReadString('\r')
		_, _ = io.WriteString(b, "\r\nNO CARRIER\r\n+CSQ: 20,99\r\nOK\r\n")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lines, err := c.Command(ctx, "AT+CSQ", nil)
	if err != nil || len(lines) != 1 || lines[0] != "+CSQ: 20,99" {
		t.Fatal(lines, err)
	}
}

// gen-extra-map generates a Go function that validates the Extra map
// included in auditevent.AuditEvent metadata for a given event index.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/elastic/go-libaudit/v2"
	"github.com/elastic/go-libaudit/v2/aucoalesce"
	"github.com/elastic/go-libaudit/v2/auparse"
)

func main() {
	log.SetFlags(0)

	err := mainWithError()
	if err != nil {
		log.Fatalln(err)
	}
}

func mainWithError() error {
	fnDescription := flag.String(
		"d",
		"",
		"The description string to include in the generated function's name\n"+
			"(e.g., 'Good')")
	numberedSessionsOnly := flag.Bool(
		"only-ids",
		false,
		"Only include auditd events with a session ID")
	output := flag.String(
		"o",
		"-",
		"The file path to write to (specify '-' for stdout)")

	flag.Parse()

	if flag.NArg() == 0 {
		return errors.New("please specify a directory containing test data files as a non-flag argument")
	}

	flag.VisitAll(func(f *flag.Flag) {
		if f.Value.String() == "" {
			log.Fatalf("please specify '-%s' - %s", f.Name, f.Usage)
		}
	})

	entries, err := os.ReadDir(flag.Arg(0))
	if err != nil {
		return err
	}

	var filePaths []string
	var readers []io.Reader

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		filePath := path.Join(flag.Arg(0), entry.Name())
		filePaths = append(filePaths, filePath)

		f, err := os.Open(filePath)
		if err != nil {
			return err
		}
		defer f.Close()

		readers = append(readers, f)
	}

	if len(readers) == 0 {
		return fmt.Errorf("no test data files were found in '%s'", flag.Arg(0))
	}

	ctx, cancelFn := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancelFn()

	const maxEventsInFlight = 1000
	const eventTimeout = 2 * time.Second
	auditdEvents := make(chan []*auparse.AuditMessage)

	reassembler, err := libaudit.NewReassembler(maxEventsInFlight, eventTimeout, &reassemblerCB{
		ctx:  ctx,
		msgs: auditdEvents,
	})
	if err != nil {
		return fmt.Errorf("failed to create new auditd message resassembler - %w", err)
	}
	defer reassembler.Close()

	go func() {
		// This code comes from the go-libaudit example in:
		// cmd/auparse/auparse.go
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()

		for range t.C {
			if reassembler.Maintain() != nil {
				// Maintain returns non-nil error
				// if reassembler was closed.
				return
			}
		}
	}()

	eventProcessorDone := make(chan error, 1)
	go func() {
		eventProcessorDone <- processAuditdEvents(io.MultiReader(readers...), reassembler)
	}()

	fnName := "metadataFor" + *fnDescription + "AuditdEvents"

	buf := bytes.NewBuffer([]byte(`// go run internal/auditd/gen-extra-map/main.go ` + strings.Join(os.Args[1:], " ") + `
//
// Code generated by the command above. DO NOT EDIT.

package auditd

import (
	"testing"

	"github.com/elastic/go-libaudit/v2/aucoalesce"
)

// ` + fnName + ` is an auto-generated function that returns
// the EventMetadata.Extra map found in the auditevent.AuditEvent object
// for the corresponding auditd event.
//
// i is the expected index of the auditevent.AuditEvent.
//
// Generated for test data files:
//   - ` + strings.Join(filePaths, "\n//   - ") + `
func ` + fnName + `(i int, t *testing.T) map[string]interface{}{
	var extra map[string]interface{}

	switch i {
`))

	i := 0

outer:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err = <-eventProcessorDone:
			if err != nil {
				return err
			}

			break outer
		case events := <-auditdEvents:
			auditdEvent, err := aucoalesce.CoalesceMessages(events)
			if err != nil {
				return fmt.Errorf("failed to coalesce auditd messages - %w", err)
			}

			if *numberedSessionsOnly && (auditdEvent.Session == "" || auditdEvent.Session == "unset") {
				continue
			}

			aucoalesce.ResolveIDs(auditdEvent)

			addCaseStatement(i, auditdEvent, buf)

			i++
		}
	}

	buf.WriteString("\tdefault:\n\t\tt.Fatalf(\"got unknown event index %d\", i)\n")
	buf.WriteString("\t}\n\n\treturn extra\n}\n")

	if *output == "-" {
		_, err = io.Copy(os.Stdout, buf)
		return err
	} else {
		const userRW = 0o600
		return os.WriteFile(*output, buf.Bytes(), userRW)
	}
}

func processAuditdEvents(r io.Reader, reass *libaudit.Reassembler) error {
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			// Parsing an empty line results in this error:
			//    invalid audit message header
			//
			// I ran into this while writing unit tests,
			// as several auditd string literal constants
			// started with a new line.
			continue
		}

		auditMsg, err := auparse.ParseLogLine(line)
		if err != nil {
			return err
		}

		reass.PushMessage(auditMsg)
	}

	return scanner.Err()
}

type reassemblerCB struct {
	ctx  context.Context
	msgs chan<- []*auparse.AuditMessage
}

func (s *reassemblerCB) ReassemblyComplete(msgs []*auparse.AuditMessage) {
	select {
	case <-s.ctx.Done():
	case s.msgs <- msgs:
	}
}

func (s *reassemblerCB) EventsLost(int) {}

func addCaseStatement(i int, event *aucoalesce.Event, buf *bytes.Buffer) {
	// case 11:
	//		extra = map[string]interface{}{
	//			"action": "ended-session",
	//			"how":    "/usr/sbin/sshd",
	//			"object": aucoalesce.Object{
	//				Type:      "user-session",
	//				Primary:   "ssh",
	//				Secondary: "127.0.0.1",
	//			},
	//		}

	buf.WriteString("\tcase ")
	buf.WriteString(strconv.Itoa(i))

	buf.WriteString(fmt.Sprintf(":\n\t\t// auditd sequence number: %d\n", event.Sequence))
	buf.WriteString("\t\textra = map[string]interface{}{\n")

	buf.WriteString(fmt.Sprintf("\t\t\t\"action\": \"%s\",\n", event.Summary.Action))
	buf.WriteString(fmt.Sprintf("\t\t\t\"how\":    \"%s\",\n", event.Summary.How))

	buf.WriteString("\t\t\t\"object\": aucoalesce.Object{\n")
	buf.WriteString(fmt.Sprintf("\t\t\t\tType:      \"%s\",\n", event.Summary.Object.Type))
	buf.WriteString(fmt.Sprintf("\t\t\t\tPrimary:   \"%s\",\n", event.Summary.Object.Primary))
	buf.WriteString(fmt.Sprintf("\t\t\t\tSecondary: \"%s\",\n", event.Summary.Object.Secondary))

	buf.WriteString("\t\t\t},\n\t\t}\n")
}
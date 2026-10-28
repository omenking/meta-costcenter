package costcenter

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// The terminal dashboard: the text report, rescanned on an interval, with keys to refresh, filter,
// regroup, and scroll. It draws with ANSI escapes in the alternate screen.

func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlGetTermios, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}

func terminalSize(f *os.File) (int, int) {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Row == 0 || ws.Col == 0 {
		return 24, 80
	}
	return int(ws.Row), int(ws.Col)
}

// rawMode puts the terminal in cbreak mode without echo and returns a function that restores it.
func rawMode(f *os.File) (func(), error) {
	var old syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlGetTermios, uintptr(unsafe.Pointer(&old))); errno != 0 {
		return nil, errno
	}
	raw := old
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN
	raw.Iflag &^= syscall.ICRNL | syscall.IXON
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlSetTermios, uintptr(unsafe.Pointer(&raw))); errno != 0 {
		return nil, errno
	}
	return func() { syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ioctlSetTermios, uintptr(unsafe.Pointer(&old))) }, nil
}

// Keys the dashboard understands, decoded from terminal input.
const (
	keyUp = iota + 0x110000
	keyDown
	keyPageUp
	keyPageDown
	keyEscape
	keyBackspace
	keyEnter
)

// readKeys decodes terminal input into keys: runes, or the constants above.
func readKeys(in io.Reader, keys chan<- rune) {
	buf := make([]byte, 256)
	for {
		n, err := in.Read(buf)
		if err != nil {
			close(keys)
			return
		}
		data := buf[:n]
		for len(data) > 0 {
			switch {
			case data[0] == 0x1b && len(data) >= 3 && (data[1] == '[' || data[1] == 'O'):
				seq := data[2:]
				consumed := 3
				switch seq[0] {
				case 'A':
					keys <- keyUp
				case 'B':
					keys <- keyDown
				case '5', '6':
					if len(seq) >= 2 && seq[1] == '~' {
						consumed = 4
						if seq[0] == '5' {
							keys <- keyPageUp
						} else {
							keys <- keyPageDown
						}
					}
				}
				data = data[consumed:]
			case data[0] == 0x1b:
				keys <- keyEscape
				data = data[1:]
			case data[0] == '\r' || data[0] == '\n':
				keys <- keyEnter
				data = data[1:]
			case data[0] == 0x7f || data[0] == '\b':
				keys <- keyBackspace
				data = data[1:]
			default:
				r, size := utf8.DecodeRune(data)
				keys <- r
				data = data[size:]
			}
		}
	}
}

// safe replaces characters that would disturb the terminal.
func safe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, s)
}

type screen struct {
	out    *os.File
	height int
	width  int
}

func (sc *screen) line(row int, text string, reverse bool) {
	text = truncate(safe(text), max(0, sc.width-1))
	if reverse {
		text = "\x1b[7m" + text + "\x1b[0m"
	}
	fmt.Fprintf(sc.out, "\x1b[%d;1H\x1b[2K%s", row+1, text)
}

// RunTUI runs the interactive terminal dashboard until q or Escape.
func RunTUI(store *Store, roots []string, config Config, filters Filters, rates []Rate, interval time.Duration) error {
	restore, err := rawMode(os.Stdin)
	if err != nil {
		return err
	}
	out := os.Stdout
	fmt.Fprint(out, "\x1b[?1049h\x1b[?25l")
	defer func() {
		fmt.Fprint(out, "\x1b[?25h\x1b[?1049l")
		restore()
	}()
	keys := make(chan rune, 64)
	go readKeys(os.Stdin, keys)
	sc := &screen{out: out}
	state := filters
	group, scroll := "model", 0
	var content []string
	var deadline time.Time
	stale, message := "", ""

	nextKey := func(wait time.Duration) (rune, bool) {
		select {
		case k, ok := <-keys:
			if !ok {
				return keyEscape, true
			}
			return k, true
		case <-time.After(wait):
			return 0, false
		}
	}
	prompt := func(label, current string) string {
		value := current
		fmt.Fprint(out, "\x1b[?25h")
		defer fmt.Fprint(out, "\x1b[?25l")
		for {
			sc.height, sc.width = terminalSize(out)
			sc.line(sc.height-1, label+": "+value, false)
			k, _ := nextKey(time.Hour)
			switch k {
			case keyEnter:
				return strings.TrimSpace(value)
			case keyEscape:
				return current
			case keyBackspace:
				if value != "" {
					_, size := utf8.DecodeLastRuneInString(value)
					value = value[:len(value)-size]
				}
			default:
				if k < 0x110000 && unicode.IsPrint(k) {
					value += string(k)
				}
			}
		}
	}

	for {
		sc.height, sc.width = terminalSize(out)
		if !time.Now().Before(deadline) {
			fmt.Fprint(out, "\x1b[2J")
			sc.line(0, "Scanning local usage logs...", false)
			result, err := store.Scan(roots, config.Sessions)
			if err == nil {
				var fresh []string
				if fresh, err = store.Lines(state, rates, group, &result); err == nil {
					content = fresh
					stale = ""
					if len(result.Warnings) > 0 {
						stale = "collection unavailable or incomplete; see warnings"
					}
				}
			}
			if err != nil {
				stale = "scan failed; showing last successful view"
			}
			deadline = time.Now().Add(interval)
		}
		sc.line(0, "q quit | r refresh | f filters | c clear | g grouping | arrows scroll", true)
		status := "Live local logs | refresh " + strconv.FormatFloat(interval.Seconds(), 'g', -1, 64) + "s | " + message
		if stale != "" {
			status = "STALE: " + stale
		}
		sc.line(1, status, false)
		body := max(0, sc.height-3)
		scroll = max(0, min(scroll, max(0, len(content)-max(1, body))))
		drawn := 0
		for ; drawn < body && scroll+drawn < len(content); drawn++ {
			sc.line(drawn+2, content[scroll+drawn], false)
		}
		// Clear whatever an earlier, longer view left below the report.
		fmt.Fprintf(out, "\x1b[%d;1H\x1b[J", drawn+3)
		k, ok := nextKey(200 * time.Millisecond)
		if !ok {
			continue
		}
		switch k {
		case 'q', keyEscape:
			return nil
		case 'r':
			deadline = time.Time{}
		case 'c':
			state = Filters{Period: "today"}
			scroll, deadline = 0, time.Time{}
		case 'g':
			options := []string{"model", "project", "session", "billing_mode"}
			for i, o := range options {
				if o == group {
					group = options[(i+1)%len(options)]
					break
				}
			}
			deadline = time.Time{}
		case 'f':
			candidate := state
			for _, field := range []struct {
				target *string
				label  string
			}{{&candidate.Period, "Period (today/week/month/all)"}, {&candidate.From, "From YYYY-MM-DD (blank clears)"},
				{&candidate.To, "To YYYY-MM-DD (blank clears)"}, {&candidate.Project, "Project (exact)"},
				{&candidate.Model, "Model (exact)"}, {&candidate.Session, "Session (includes descendants)"},
				{&candidate.BillingMode, "Billing mode (payg/subscription/unknown)"}} {
				*field.target = prompt(field.label, *field.target)
			}
			if candidate.Period == "" {
				candidate.Period = "today"
			}
			_, _, err := bounds(candidate)
			switch candidate.BillingMode {
			case "", "payg", "subscription", "unknown":
			default:
				err = fmt.Errorf("invalid billing mode")
			}
			if err != nil {
				message = "Invalid filters; previous filters retained"
			} else {
				state, message, scroll = candidate, "", 0
			}
			deadline = time.Time{}
		case keyDown, 'j':
			scroll++
		case keyUp, 'k':
			scroll = max(0, scroll-1)
		case keyPageDown:
			scroll += max(1, sc.height-3)
		case keyPageUp:
			scroll = max(0, scroll-sc.height+3)
		}
	}
}

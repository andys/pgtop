//go:build linux || darwin

package term

import (
	"os"
	"syscall"
	"unsafe"
)

// Term manages raw terminal mode and provides terminal dimensions.
type Term struct {
	origTermios syscall.Termios
	width       int
	height      int
}

// New puts the terminal into raw mode and returns a Term.
func New() (*Term, error) {
	t := &Term{}

	// Save original terminal settings
	if err := tcgetattr(syscall.Stdin, &t.origTermios); err != nil {
		return nil, err
	}

	// Set raw mode
	raw := t.origTermios
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	// Keep OPOST enabled so kernel translates \n to \r\n
	// raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0

	if err := tcsetattr(syscall.Stdin, &raw); err != nil {
		return nil, err
	}

	// Hide cursor
	os.Stdout.WriteString("\033[?25l")

	t.UpdateSize()
	return t, nil
}

// Restore restores the original terminal settings.
func (t *Term) Restore() {
	// Show cursor
	os.Stdout.WriteString("\033[?25h")
	// Clear screen
	os.Stdout.WriteString("\033[2J\033[H")
	tcsetattr(syscall.Stdin, &t.origTermios)
}

// Size returns the current terminal width and height.
func (t *Term) Size() (int, int) {
	return t.width, t.height
}

// UpdateSize refreshes the cached terminal dimensions.
func (t *Term) UpdateSize() {
	w, h := getTermSize()
	if w > 0 {
		t.width = w
	}
	if h > 0 {
		t.height = h
	}
	if t.width == 0 {
		t.width = 80
	}
	if t.height == 0 {
		t.height = 24
	}
}

type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func getTermSize() (int, int) {
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(syscall.Stdout),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}

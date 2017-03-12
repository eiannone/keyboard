// +build !windows

package keyboard

import (
    "os"
    "os/signal"
    "syscall"
    "unicode/utf8"
    "strings"
    "fmt"
    "runtime"
    "unsafe"
)

type (
    input_event struct {
        data []byte
        err  error
    }

    syscall_Termios syscall.Termios
)

const (
    syscall_IGNBRK = syscall.IGNBRK
    syscall_BRKINT = syscall.BRKINT
    syscall_PARMRK = syscall.PARMRK
    syscall_ISTRIP = syscall.ISTRIP
    syscall_INLCR  = syscall.INLCR
    syscall_IGNCR  = syscall.IGNCR
    syscall_ICRNL  = syscall.ICRNL
    syscall_IXON   = syscall.IXON
    syscall_ECHO   = syscall.ECHO
    syscall_ECHONL = syscall.ECHONL
    syscall_ICANON = syscall.ICANON
    syscall_ISIG   = syscall.ISIG
    syscall_IEXTEN = syscall.IEXTEN
    syscall_CSIZE  = syscall.CSIZE
    syscall_PARENB = syscall.PARENB
    syscall_CS8    = syscall.CS8
    syscall_VMIN   = syscall.VMIN
    syscall_VTIME  = syscall.VTIME

    syscall_TCGETS = syscall.TCGETS
    syscall_TCSETS = syscall.TCSETS
)

var (
    out   *os.File
    in    int

    // term specific keys
    keys  []string

    // termbox inner state
    orig_tios      syscall_Termios

    sigio      = make(chan os.Signal, 1)
    quit       = make(chan int)
    inbuf      = make([]byte, 0, 64)
    input_buf  = make(chan input_event)
)

func fcntl(fd int, cmd int, arg int) (val int, err error) {
    r, _, e := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(cmd), uintptr(arg))
    val = int(r)
    if e != 0 {
        err = e
    }
    return
}

func tcsetattr(fd uintptr, termios *syscall_Termios) error {
    r, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall_TCSETS), uintptr(unsafe.Pointer(termios)))
    if r != 0 {
        return os.NewSyscallError("SYS_IOCTL", e)
    }
    return nil
}

func parse_escape_sequence(buf []byte) (size int, event keyEvent) {
    bufstr := string(buf)
    for i, key := range keys {
        if strings.HasPrefix(bufstr, key) {
            event.rune = 0
            event.key = Key(0xFFFF - i)
            size = len(key)
            return
        }
    }
    return 0, event
}

func extract_event(inbuf []byte) int {
    if len(inbuf) == 0 {
        return 0
    }

    if inbuf[0] == '\033' {
        // possible escape sequence
        if size, event := parse_escape_sequence(inbuf); size != 0 {
            input_comm <- event
            return size
        }

        // it's not escape sequence, then it's Esc event
        input_comm <- keyEvent{key: KeyEsc}
        return 1
    }

    // if we're here, this is not an escape sequence and not an alt sequence
    // so, it's a FUNCTIONAL KEY or a UNICODE character

    // first of all check if it's a functional key
    if Key(inbuf[0]) <= KeySpace || Key(inbuf[0]) == KeyBackspace2 {
        input_comm <- keyEvent{key: Key(inbuf[0])}
        return 1
    }

    // the only possible option is utf8 rune
    if r, n := utf8.DecodeRune(inbuf); r != utf8.RuneError {
        input_comm <- keyEvent{rune: r}
        return n
    }

    return 0
}

// Wait for an event and return it. This is a blocking function call.
func inputEventsProducer() {
    // try to extract event from input buffer, return on success
    size := extract_event(inbuf)
    if size != 0 {
        copy(inbuf, inbuf[size:])
        inbuf = inbuf[:len(inbuf)-size]
    }

    for {
        select {
        case ev := <-input_buf:
            if ev.err != nil {
                input_comm <- keyEvent{err: ev.err}
                return
            }

            inbuf = append(inbuf, ev.data...)
            input_buf <- ev
            size = extract_event(inbuf)
            if size != 0 {
                copy(inbuf, inbuf[size:])
                inbuf = inbuf[:len(inbuf)-size]
            }
        case <-quit:
            return
        }
    }
}

// Credits to: http://stackoverflow.com/a/17278730/939269
func initConsole() (err error) {
    out, err = os.OpenFile("/dev/tty", syscall.O_WRONLY, 0)
    if err != nil {
        return
    }
    in, err = syscall.Open("/dev/tty", syscall.O_RDONLY, 0)
    if err != nil {
        return
    }

    err = setup_term()
    if err != nil {
        return fmt.Errorf("Error while reading terminfo data: %v", err)
    }

    signal.Notify(sigio, syscall.SIGIO)

    _, err = fcntl(in, syscall.F_SETFL, syscall.O_ASYNC|syscall.O_NONBLOCK)
    if err != nil {
        return
    }
    _, err = fcntl(in, syscall.F_SETOWN, syscall.Getpid())
    if runtime.GOOS != "darwin" && err != nil {
        return err
    }

    r, _, e := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), uintptr(syscall_TCGETS), uintptr(unsafe.Pointer(&orig_tios)))
    if r != 0 {
        return os.NewSyscallError("SYS_IOCTL", e)
    }

    tios := orig_tios
    tios.Iflag &^= syscall_IGNBRK | syscall_BRKINT | syscall_PARMRK |
        syscall_ISTRIP | syscall_INLCR | syscall_IGNCR |
        syscall_ICRNL | syscall_IXON
    tios.Lflag &^= syscall_ECHO | syscall_ECHONL | syscall_ICANON |
        syscall_ISIG | syscall_IEXTEN
    tios.Cflag &^= syscall_CSIZE | syscall_PARENB
    tios.Cflag |= syscall_CS8
    tios.Cc[syscall_VMIN] = 1
    tios.Cc[syscall_VTIME] = 0

    err = tcsetattr(out.Fd(), &tios)
    if err != nil {
        return err
    }

    go func() {
        buf := make([]byte, 128)
        for {
            select {
            case <-sigio:
                for {
                    n, err := syscall.Read(in, buf)
                    if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
                        break
                    }
                    select {
                    case input_buf <- input_event{buf[:n], err}:
                        ie := <-input_buf
                        buf = ie.data[:128]
                    case <-quit:
                        return
                    }
                }
            case <-quit:
                return
            }
        }
    }()

    go inputEventsProducer()
    return
}

func releaseConsole() {
    quit <- 1
    tcsetattr(out.Fd(), &orig_tios)
    out.Close()
    syscall.Close(in)
}

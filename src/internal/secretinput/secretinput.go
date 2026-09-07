// Package secretinput reads interactive credentials without terminal echo.
package secretinput

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// ReadLine prints prompt and reads one line. Interactive terminal input fails
// closed unless echo can be disabled and restored; redirected input remains
// available for answer files and other deliberate automation.
func ReadLine(out io.Writer, prompt string, input *os.File, reader *bufio.Reader) (string, error) {
	if input == nil || reader == nil {
		return "", fmt.Errorf("secret input is unavailable")
	}
	fmt.Fprint(out, prompt)
	info, err := input.Stat()
	if err != nil {
		return "", fmt.Errorf("inspect secret input: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		line, err := reader.ReadString('\n')
		fmt.Fprintln(out)
		return line, err
	}

	fd := int(input.Fd())
	original, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if errors.Is(err, unix.ENOTTY) {
		line, err := reader.ReadString('\n')
		fmt.Fprintln(out)
		return line, err
	}
	if err != nil {
		return "", fmt.Errorf("disable terminal echo: %w", err)
	}
	hidden := *original
	hidden.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &hidden); err != nil {
		return "", fmt.Errorf("disable terminal echo: %w", err)
	}
	line, readErr := reader.ReadString('\n')
	restoreErr := unix.IoctlSetTermios(fd, unix.TCSETS, original)
	fmt.Fprintln(out)
	if restoreErr != nil {
		return "", fmt.Errorf("restore terminal echo: %w", restoreErr)
	}
	return line, readErr
}

package secretinput

import (
	"bufio"
	"bytes"
	"os"
	"testing"
)

func TestReadLineFromRedirectedInput(t *testing.T) {
	input, err := os.CreateTemp(t.TempDir(), "answers-*")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.WriteString("private-value\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	line, err := ReadLine(&output, "Secret: ", input, bufio.NewReader(input))
	if err != nil {
		t.Fatalf("read redirected secret: %v", err)
	}
	if line != "private-value\n" {
		t.Fatalf("line = %q", line)
	}
	if output.String() != "Secret: \n" {
		t.Fatalf("prompt output = %q", output.String())
	}
}

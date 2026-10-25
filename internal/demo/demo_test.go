package demo

import (
	"io"
	"log"
	"os"
	"strings"
	"testing"
)

func TestDemoCompletesAllScenarios(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousOutput, previousLog := os.Stdout, log.Writer()
	os.Stdout = writer
	log.SetOutput(io.Discard)
	t.Cleanup(func() { os.Stdout = previousOutput; log.SetOutput(previousLog); reader.Close(); writer.Close() })
	output := make(chan string, 1)
	go func() { data, _ := io.ReadAll(reader); output <- string(data) }()
	Run()
	writer.Close()
	text := <-output
	for _, expected := range []string{"Order cancelled successfully", "Final Engine Statistics:", "Stop-Loss Order Demo", "Risk Management Statistics:", "=== Demo Complete ===", "$50.000000"} {
		if !strings.Contains(text, expected) {
			t.Errorf("demo did not report %q", expected)
		}
	}
	if strings.Contains(text, "%!s") || strings.Contains(text, "%!d") {
		t.Fatal("demo contains invalid formatted output")
	}
}

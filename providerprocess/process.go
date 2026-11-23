// Package providerprocess owns native stdio agent processes without a PTY.
package providerprocess

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type Options struct {
	Command         []string
	Directory       string
	Env             []string
	Stderr          io.Writer
	ShutdownTimeout time.Duration
}
type Process struct {
	command  *exec.Cmd
	stdout   *os.File
	stdin    *os.File
	done     chan struct{}
	once     sync.Once
	err      error
	shutdown time.Duration
	cleanup  func()
	kill     func() error
}

// ValidateDirectory returns the absolute path to an existing working directory.
func ValidateDirectory(directory, provider string) (string, error) {
	if directory == "" {
		return "", fmt.Errorf("%s working directory is required", provider)
	}
	abs, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve %s working directory: %w", provider, err)
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s working directory must exist", provider)
	}
	return abs, nil
}

func Start(ctx context.Context, options Options) (*Process, error) {
	if len(options.Command) == 0 || options.Command[0] == "" {
		return nil, fmt.Errorf("provider command is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command := exec.Command(options.Command[0], options.Command[1:]...)
	command.Dir = options.Directory
	command.Env = options.Env
	if options.Stderr != nil {
		command.Stderr = options.Stderr
	} else {
		command.Stderr = io.Discard
	}
	command.WaitDelay = 250 * time.Millisecond
	configureProcess(command)
	inReader, inWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outReader, outWriter, err := os.Pipe()
	if err != nil {
		inReader.Close()
		inWriter.Close()
		return nil, err
	}
	command.Stdin = inReader
	command.Stdout = outWriter
	if err := command.Start(); err != nil {
		inReader.Close()
		inWriter.Close()
		outReader.Close()
		outWriter.Close()
		return nil, fmt.Errorf("start provider process: %w", err)
	}
	inReader.Close()
	outWriter.Close()
	cleanup, kill, err := ownProcess(command)
	if err != nil {
		command.Process.Kill()
		command.Wait()
		inWriter.Close()
		outReader.Close()
		return nil, err
	}
	timeout := options.ShutdownTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	p := &Process{command: command, stdin: inWriter, stdout: outReader, done: make(chan struct{}), shutdown: timeout, cleanup: cleanup, kill: kill}
	// The pipe endpoints are ours, not StdoutPipe: Wait cannot close stdout before
	// the RPC reader has consumed a final buffered response from an exiting agent.
	go func() { p.err = command.Wait(); close(p.done) }()
	go func() {
		select {
		case <-ctx.Done():
			p.Close()
		case <-p.done:
		}
	}()
	return p, nil
}
func (p *Process) Read(data []byte) (int, error)  { return p.stdout.Read(data) }
func (p *Process) Write(data []byte) (int, error) { return p.stdin.Write(data) }
func (p *Process) Done() <-chan struct{}          { return p.done }
func (p *Process) Close() error {
	p.once.Do(func() {
		p.stdin.Close() // Protocol EOF gives the provider time to save its own session.
		timer := time.NewTimer(p.shutdown)
		defer timer.Stop()
		select {
		case <-p.done:
		case <-timer.C:
			p.kill()
			<-p.done
		}
		p.stdout.Close()
		p.cleanup()
	})
	return p.err
}

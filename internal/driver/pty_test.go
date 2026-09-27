package driver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// Regression for the full-duplex requirement: a goroutine parked in a
// blocking read must never stop a concurrent write from reaching the agent.
// The old single-mutex handle deadlocked this exact shape (reader waits for
// output, agent waits for input, writer waits for the reader).
func TestBlockingReadDoesNotBlockWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Driver
	}{
		{"pty", NewPTYDriver()},
		{"pipe", NewProcessDriver()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := tc.d.Start(context.Background(), Spec{Path: "sh"})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			t.Cleanup(func() {
				stopped := make(chan error, 1)
				go func() { stopped <- tc.d.Stop(context.Background(), h) }()
				tc.d.Wait(h) // close done so Stop settles the moment the process dies
				<-stopped
			})

			got := make(chan struct{})
			go func() {
				defer close(got)
				var seen strings.Builder
				buf := make([]byte, 256)
				for {
					n, err := tc.d.Read(h, buf)
					if n > 0 {
						seen.Write(buf[:n])
						if strings.Contains(seen.String(), "hello") {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
			// Let the reader be the one to get blocked first.
			time.Sleep(100 * time.Millisecond)

			wrote := make(chan error, 1)
			go func() {
				_, err := tc.d.Write(h, []byte("echo hello\n"))
				wrote <- err
			}()

			select {
			case err := <-wrote:
				if err != nil {
					t.Fatalf("Write: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("deadlock: blocking Read held a lock Write needed")
			}
			<-got
		})
	}
}

// A background job running under interactive job control gets its own process
// group inside the terminal's session. Stop must reach that group too, not
// just the session leader's: otherwise the job survives a stopped shell.
func TestStopKillsJobControlProcessGroup(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available for job-control setup")
	}

	d := NewPTYDriver()
	h, err := d.Start(context.Background(), Spec{Path: bash})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The manager-equivalent: a monitor owns Wait concurrently with Stop.
	wres := make(chan ExitResult, 1)
	go func() { wres <- d.Wait(h) }()

	pidFile := fmt.Sprintf("%s/job.pid", t.TempDir())
	// Interactive bash puts background jobs in their own process group;
	// echo $! captures the job pid so the test can inspect and assert it.
	if _, err := d.Write(h, []byte(fmt.Sprintf("sleep 1000 & echo $! > %s\n", pidFile))); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var raw string
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			raw = strings.TrimSpace(string(b))
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job pid file never appeared: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("bad job pid %q: %v", raw, err)
	}
	if !processAlive(job) {
		t.Fatalf("job %d not running before stop", job)
	}

	// Sanity: the job is in a real job-control group inside the session.
	// If the shell didn't create a separate group, this test can't assert
	// anything about cross-group teardown.
	sid, pgrp, err := procSessionGroup(job)
	if err != nil {
		t.Fatalf("read job stat: %v", err)
	}
	if sid != h.PID {
		t.Fatalf("job session %d != pty session %d", sid, h.PID)
	}
	if pgrp == sid {
		t.Skip("shell did not put the job in its own process group")
	}

	if err := d.Stop(context.Background(), h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if processAlive(job) {
		t.Fatalf("job %d (own process group) survived the session stop", job)
	}
	if res := <-wres; res.Signal != syscall.SIGTERM && res.Signal != syscall.SIGKILL {
		t.Fatalf("shell did not die by a stop signal: %+v", res)
	}
}

func processAlive(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(raw)
	fields := strings.Fields(s[strings.LastIndex(s, ")")+1:])
	if len(fields) == 0 {
		return false
	}
	return fields[0] != "Z"
}

// procSessionGroup returns the session id and process group of pid.
func procSessionGroup(pid int) (sid, pgrp int, err error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, err
	}
	s := string(raw)
	fields := strings.Fields(s[strings.LastIndex(s, ")")+1:])
	if len(fields) < 4 {
		return 0, 0, fmt.Errorf("stat has %d fields", len(fields))
	}
	pgrp, err = strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, err
	}
	sid, err = strconv.Atoi(fields[3])
	if err != nil {
		return 0, 0, err
	}
	return sid, pgrp, nil
}

func TestReadTimeoutDrainsThenGoesQuiet(t *testing.T) {
	d := NewPTYDriver()
	h, err := d.Start(context.Background(), Spec{Path: "sh"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer d.Stop(context.Background(), h)

	if _, err := d.Write(h, []byte("echo hi\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	buf := make([]byte, 256)
	var got strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(got.String(), "hi") && time.Now().Before(deadline) {
		n, err := d.ReadTimeout(h, buf, 300*time.Millisecond)
		if err != nil {
			t.Fatalf("ReadTimeout: %v", err)
		}
		got.Write(buf[:n])
	}
	if !strings.Contains(got.String(), "hi") {
		t.Fatalf("output %q does not contain hi", got.String())
	}

	// Drain everything until the process actually goes quiet (the shell
	// prompt is usually still in flight right after the echo).
	for {
		n, err := d.ReadTimeout(h, buf, 200*time.Millisecond)
		if err != nil {
			t.Fatalf("drain ReadTimeout: %v", err)
		}
		if n == 0 {
			break
		}
		got.Write(buf[:n])
	}

	// Output is drained now; bash sits at its prompt, so the read must
	// return (0, nil) after the timeout instead of blocking forever.
	start := time.Now()
	n, err := d.ReadTimeout(h, buf, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("quiet ReadTimeout: %v", err)
	}
	if n != 0 {
		t.Fatalf("quiet read returned %d bytes: %q", n, buf[:n])
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("quiet read returned too early: %v", elapsed)
	}
}

func TestPTYDriverInteractiveShell(t *testing.T) {
	d := NewPTYDriver()
	h, err := d.Start(context.Background(), Spec{Path: "sh"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer d.Stop(context.Background(), h)
	if h.PID == 0 {
		t.Fatal("expected a real PID")
	}
	if h.master == nil {
		t.Fatal("expected the handle to carry a PTY master")
	}

	output := make(chan string, 1)
	go func() {
		var got strings.Builder
		buf := make([]byte, 256)
		for {
			n, err := d.Read(h, buf)
			if n > 0 {
				got.Write(buf[:n])
				if strings.Contains(got.String(), "hello") {
					output <- got.String()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	if _, err := d.Write(h, []byte("echo hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case got := <-output:
		if !strings.Contains(got, "hello") {
			t.Fatalf("terminal output %q does not contain hello", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the shell to echo output")
	}

	if err := d.Resize(h, 30, 90); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	rows, cols, err := pty.Getsize(h.master)
	if err != nil {
		t.Fatalf("Getsize: %v", err)
	}
	if rows != 30 || cols != 90 {
		t.Fatalf("winsize = %dx%d, want 30x90", rows, cols)
	}

	if _, err := d.Write(h, []byte("exit\n")); err != nil {
		t.Fatalf("Write exit: %v", err)
	}
	res := d.Wait(h)
	if res.Err != nil {
		t.Fatalf("expected a clean exit, got: %v", res.Err)
	}
}

func TestStopEscalatesWhenJobControlChildIgnoresSIGTERM(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}

	d := NewPTYDriver()
	d.Grace = 150 * time.Millisecond

	h, err := d.Start(
		context.Background(),
		Spec{
			Path: bash,
			Args: []string{
				"--noprofile",
				"--norc",
				"-i",
			},
		},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitDone := make(chan ExitResult, 1)
	go func() {
		waitDone <- d.Wait(h)
	}()

	pidFile := fmt.Sprintf(
		"%s/stubborn.pid",
		t.TempDir(),
	)

	// Background job:
	// - stays inside the PTY session
	// - receives its own job-control process group
	// - deliberately ignores SIGTERM
	cmd := fmt.Sprintf(
		"bash -c 'trap \"\" TERM; exec sleep 1000' & echo $! > %q\n",
		pidFile,
	)

	if _, err := d.Write(h, []byte(cmd)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var childPID int
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			childPID, err = strconv.Atoi(
				strings.TrimSpace(string(raw)),
			)
			if err == nil {
				break
			}
		}

		time.Sleep(10 * time.Millisecond)
	}

	if childPID == 0 {
		t.Fatal("TERM-ignoring child PID was not produced")
	}

	if !processAlive(childPID) {
		t.Fatalf("child %d is not alive before Stop", childPID)
	}

	sid, pgid, err := procSessionGroup(childPID)
	if err != nil {
		t.Fatalf("procSessionGroup: %v", err)
	}

	if sid != h.PID {
		t.Fatalf(
			"child session = %d, want %d",
			sid,
			h.PID,
		)
	}

	if pgid == h.PID {
		t.Skip(
			"shell did not place background job in a separate process group",
		)
	}

	started := time.Now()

	if err := d.Stop(context.Background(), h); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	elapsed := time.Since(started)

	// Since the child ignored TERM, the grace period must have elapsed
	// before SIGKILL could terminate it.
	if elapsed < d.Grace {
		t.Fatalf(
			"Stop returned in %v before grace period %v; "+
				"likely returned when only the session leader died",
			elapsed,
			d.Grace,
		)
	}

	if processAlive(childPID) {
		t.Fatalf(
			"TERM-ignoring job-control child %d survived Stop",
			childPID,
		)
	}

	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("PTY leader was not reaped")
	}
}

/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/docker/docker/pkg/jsonlog"
	"github.com/fsnotify/fsnotify"
	"github.com/golang/glog"
	"github.com/urfave/cli"
	"golang.org/x/net/context"
	"k8s.io/api/core/v1"
	pb "k8s.io/kubernetes/pkg/kubelet/apis/cri/v1alpha1/runtime"
	"k8s.io/kubernetes/pkg/util/tail"
)

// streamType is the type of the stream.
type streamType string

const (
	stderrType streamType = "stderr"
	stdoutType streamType = "stdout"

	// timeFormat is the time format used in the log.
	timeFormat = time.RFC3339Nano
)

var (
	// eol is the end-of-line sign in the log.
	eol = []byte{'\n'}
	// delimiter is the delimiter for timestamp and streamtype in log line.
	delimiter = []byte{' '}
)

// logMessage is the internal log type.
type logMessage struct {
	timestamp time.Time
	stream    streamType
	log       []byte
}

// reset resets the log to nil.
func (l *logMessage) reset() {
	l.timestamp = time.Time{}
	l.stream = ""
	l.log = nil
}

// logOptions is the internal type of all log options.
type logOptions struct {
	tail      int64
	bytes     int64
	since     time.Time
	follow    bool
	timestamp bool
}

var logsCommand = cli.Command{
	Name:      "logs",
	Usage:     "Fetch the logs of a container",
	ArgsUsage: "CONTAINER",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "follow, f",
			Usage: "Follow log output",
		},
		cli.Int64Flag{
			Name:  "tail, t",
			Value: -1,
			Usage: "Number of lines to show from the end of the logs. Defaults to all",
		},
		cli.Int64Flag{
			Name:  "limit-bytes",
			Value: -1,
			Usage: "Maximum bytes of logs to return. Defaults to no limit",
		},
	},
	Action: func(context *cli.Context) error {
		if err := getRuntimeClient(context); err != nil {
			return err
		}
		tailLines := context.Int64("tail")
		limitBytes := context.Int64("limit-bytes")
		logOptions := &v1.PodLogOptions{
			Follow:     context.Bool("follow"),
			TailLines:  &tailLines,
			LimitBytes: &limitBytes,
		}
		r, err := getContainerStatus(runtimeClient, context.Args().First())
		if err != nil {
			return err
		}
		logPath := r.Status.GetLogPath()
		if logPath == "" {
			return fmt.Errorf("Get log path of container failed")
		}
		return ReadLogs(logPath, logOptions, os.Stdout, os.Stderr)
	},
	After: closeConnection,
}

func getContainerStatus(client pb.RuntimeServiceClient, ID string) (*pb.ContainerStatusResponse, error) {
	if ID == "" {
		return nil, fmt.Errorf("ID cannot be empty")
	}
	request := &pb.ContainerStatusRequest{
		ContainerId: ID,
	}
	r, err := client.ContainerStatus(context.Background(), request)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// newLogOptions convert the v1.PodLogOptions to internal logOptions.
func newLogOptions(apiOpts *v1.PodLogOptions, now time.Time) *logOptions {
	opts := &logOptions{
		tail:      *apiOpts.TailLines,
		bytes:     *apiOpts.LimitBytes,
		follow:    apiOpts.Follow,
		timestamp: apiOpts.Timestamps,
	}
	if apiOpts.TailLines != nil {
		opts.tail = *apiOpts.TailLines
	}
	if apiOpts.LimitBytes != nil {
		opts.bytes = *apiOpts.LimitBytes
	}
	if apiOpts.SinceSeconds != nil {
		opts.since = now.Add(-time.Duration(*apiOpts.SinceSeconds) * time.Second)
	}
	if apiOpts.SinceTime != nil && apiOpts.SinceTime.After(opts.since) {
		opts.since = apiOpts.SinceTime.Time
	}
	return opts
}

// ReadLogs read the container log and redirect into stdout and stderr.
func ReadLogs(path string, apiOpts *v1.PodLogOptions, stdout, stderr io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("failed to open log file %q: %v", path, err)
	}
	defer f.Close()

	// Convert v1.PodLogOptions into internal log options.
	opts := newLogOptions(apiOpts, time.Now())

	// Search start point based on tail line.
	start, err := tail.FindTailLineStartIndex(f, opts.tail)
	if err != nil {
		return fmt.Errorf("failed to tail %d lines of log file %q: %v", opts.tail, path, err)
	}
	if _, err := f.Seek(start, os.SEEK_SET); err != nil {
		return fmt.Errorf("failed to seek %d in log file %q: %v", start, path, err)
	}

	// Start parsing the logs.
	r := bufio.NewReader(f)
	// Do not create watcher here because it is not needed if `Follow` is false.
	var watcher *fsnotify.Watcher
	var parse parseFunc
	writer := newLogWriter(stdout, stderr, opts)
	msg := &logMessage{}
	for {
		l, err := r.ReadBytes(eol[0])
		if err != nil {
			if err != io.EOF { // This is an real error
				return fmt.Errorf("failed to read log file %q: %v", path, err)
			}
			if !opts.follow {
				// Return directly when reading to the end if not follow.
				if len(l) > 0 {
					glog.Warningf("Incomplete line in log file %q: %q", path, l)
				}
				glog.V(2).Infof("Finish parsing log file %q", path)
				return nil
			}
			// Reset seek so that if this is an incomplete line,
			// it will be read again.
			if _, err = f.Seek(-int64(len(l)), os.SEEK_CUR); err != nil {
				return fmt.Errorf("failed to reset seek in log file %q: %v", path, err)
			}
			if watcher == nil {
				// Intialize the watcher if it has not been initialized yet.
				if watcher, err = fsnotify.NewWatcher(); err != nil {
					return fmt.Errorf("failed to create fsnotify watcher: %v", err)
				}
				defer watcher.Close()
				if err = watcher.Add(f.Name()); err != nil {
					return fmt.Errorf("failed to watch file %q: %v", f.Name(), err)
				}
			}
			// Wait until the next log change.
			if err = waitLogs(watcher); err != nil {
				return fmt.Errorf("failed to wait logs for log file %q: %v", path, err)
			}
			continue
		}
		if parse == nil {
			// Intialize the log parsing function.
			parse, err = getParseFunc(l)
			if err != nil {
				return fmt.Errorf("failed to get parse function: %v", err)
			}
		}
		// Parse the log line.
		msg.reset()
		if err := parse(l, msg); err != nil {
			glog.Errorf("Failed with err %v when parsing log for log file %q: %q", err, path, l)
			continue
		}
		// Write the log line into the stream.
		if err := writer.write(msg); err != nil {
			if err == errMaximumWrite {
				glog.V(2).Infof("Finish parsing log file %q, hit bytes limit %d(bytes)", path, opts.bytes)
				return nil
			}
			glog.Errorf("Failed with err %v when writing log for log file %q: %+v", err, path, msg)
			return err
		}
	}
}

// parseFunc is a function parsing one log line to the internal log type.
// Notice that the caller must make sure logMessage is not nil.
type parseFunc func([]byte, *logMessage) error

var parseFuncs = []parseFunc{
	parseCRILog,        // CRI log format parse function
	parseDockerJSONLog, // Docker JSON log format parse function
}

// parseCRILog parses logs in CRI log format. CRI Log format example:
//   2016-10-06T00:17:09.669794202Z stdout log content 1
//   2016-10-06T00:17:09.669794203Z stderr log content 2
func parseCRILog(log []byte, msg *logMessage) error {
	var err error
	// Parse timestamp
	idx := bytes.Index(log, delimiter)
	if idx < 0 {
		return fmt.Errorf("timestamp is not found")
	}
	msg.timestamp, err = time.Parse(timeFormat, string(log[:idx]))
	if err != nil {
		return fmt.Errorf("unexpected timestamp format %q: %v", timeFormat, err)
	}

	// Parse stream type
	log = log[idx+1:]
	idx = bytes.Index(log, delimiter)
	if idx < 0 {
		return fmt.Errorf("stream type is not found")
	}
	msg.stream = streamType(log[:idx])
	if msg.stream != stdoutType && msg.stream != stderrType {
		return fmt.Errorf("unexpected stream type %q", msg.stream)
	}

	// Get log content
	msg.log = log[idx+1:]

	return nil
}

// dockerJSONLog is the JSON log buffer used in parseDockerJSONLog.
var dockerJSONLog = &jsonlog.JSONLog{}

// parseDockerJSONLog parses logs in Docker JSON log format. Docker JSON log format
// example:
//   {"log":"content 1","stream":"stdout","time":"2016-10-20T18:39:20.57606443Z"}
//   {"log":"content 2","stream":"stderr","time":"2016-10-20T18:39:20.57606444Z"}
func parseDockerJSONLog(log []byte, msg *logMessage) error {
	dockerJSONLog.Reset()
	l := dockerJSONLog
	// TODO: JSON decoding is fairly expensive, we should evaluate this.
	if err := json.Unmarshal(log, l); err != nil {
		return fmt.Errorf("failed with %v to unmarshal log %q", err, l)
	}
	msg.timestamp = l.Created
	msg.stream = streamType(l.Stream)
	msg.log = []byte(l.Log)
	return nil
}

// getParseFunc returns proper parse function based on the sample log line passed in.
func getParseFunc(log []byte) (parseFunc, error) {
	for _, p := range parseFuncs {
		if err := p(log, &logMessage{}); err == nil {
			return p, nil
		}
	}
	return nil, fmt.Errorf("unsupported log format: %q", log)
}

// waitLogs wait for the next log write.
func waitLogs(w *fsnotify.Watcher) error {
	errRetry := 5
	for {
		select {
		case e := <-w.Events:
			switch e.Op {
			case fsnotify.Write:
				return nil
			default:
				glog.Errorf("Unexpected fsnotify event: %v, retrying...", e)
			}
		case err := <-w.Errors:
			glog.Errorf("Fsnotify watch error: %v, %d error retries remaining", err, errRetry)
			if errRetry == 0 {
				return err
			}
			errRetry--
		}
	}
}

// logWriter controls the writing into the stream based on the log options.
type logWriter struct {
	stdout io.Writer
	stderr io.Writer
	opts   *logOptions
	remain int64
}

// errMaximumWrite is returned when all bytes have been written.
var errMaximumWrite = errors.New("maximum write")

// errShortWrite is returned when the message is not fully written.
var errShortWrite = errors.New("short write")

func newLogWriter(stdout io.Writer, stderr io.Writer, opts *logOptions) *logWriter {
	w := &logWriter{
		stdout: stdout,
		stderr: stderr,
		opts:   opts,
		remain: math.MaxInt64, // initialize it as infinity
	}
	if opts.bytes >= 0 {
		w.remain = opts.bytes
	}
	return w
}

// writeLogs writes logs into stdout, stderr.
func (w *logWriter) write(msg *logMessage) error {
	if msg.timestamp.Before(w.opts.since) {
		// Skip the line because it's older than since
		return nil
	}
	line := msg.log
	if w.opts.timestamp {
		prefix := append([]byte(msg.timestamp.Format(timeFormat)), delimiter[0])
		line = append(prefix, line...)
	}
	// If the line is longer than the remaining bytes, cut it.
	if int64(len(line)) > w.remain {
		line = line[:w.remain]
	}
	// Get the proper stream to write to.
	var stream io.Writer
	switch msg.stream {
	case stdoutType:
		stream = w.stdout
	case stderrType:
		stream = w.stderr
	default:
		return fmt.Errorf("unexpected stream type %q", msg.stream)
	}
	n, err := stream.Write(line)
	w.remain -= int64(n)
	if err != nil {
		return err
	}
	// If the line has not been fully written, return errShortWrite
	if n < len(line) {
		return errShortWrite
	}
	// If there are no more bytes left, return errMaximumWrite
	if w.remain <= 0 {
		return errMaximumWrite
	}
	return nil
}

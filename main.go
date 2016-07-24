package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"text/template"

	"github.com/alexflint/go-arg"
	"github.com/brentp/xopen"
)

// Version is the current version
const Version = "0.3.2"

// ExitCode is the highest exit code seen in any command
var ExitCode = 0

// Params are the user-specified command-line arguments
type Params struct {
	Procs           int    `arg:"-p,help:number of processes to use"`
	Nlines          int    `arg:"-n,help:number of lines to consume for each command. -s and -n are mutually exclusive."`
	Command         string `arg:"positional,required,help:command to execute"`
	Sep             string `arg:"-s,help:regular expression split line with to fill multiple template spots default is not to split. -s and -n are mutually exclusive."`
	Verbose         bool   `arg:"-v,help:print commands to stderr before they are executed."`
	ContinueOnError bool   `arg:"-c,--continue-on-error,help:report errors but don't stop the entire execution (which is the default)."`
	DryRun          bool   `arg:"-d,--dry-run,help:print (but do not run) the commands"`
}

// hold the arguments for each call that fill the template.
type tmplArgs struct {
	Lines []string
	Xs    []string
}

func main() {
	args := Params{Procs: 1, Nlines: 1}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "bash"
	}
	p := arg.MustParse(&args)
	if args.Sep != "" && args.Nlines > 1 {
		p.Fail("must specify either sep (-s) or n-lines (-n), not both")
	}
	if !xopen.IsStdin() {
		fmt.Fprintln(os.Stderr, "ERROR: expecting input on STDIN")
		os.Exit(255)
	}
	runtime.GOMAXPROCS(args.Procs)
	run(args, shell)
	os.Exit(ExitCode)
}

func check(e error) {
	if e != nil {
		log.Fatal(e)
	}
}

func genTmplArgs(n int, sep string) chan *tmplArgs {
	ch := make(chan *tmplArgs)
	var resep *regexp.Regexp
	if sep != "" {
		resep = regexp.MustCompile(sep)
	}

	go func() {
		rdr, err := xopen.Ropen("-")
		check(err)
		k := 0
		re := regexp.MustCompile(`\r?\n`)
		lines := make([]string, n)

		for {
			line, err := rdr.ReadString('\n')
			if err == nil || (err == io.EOF && len(line) > 0) {
				line = re.ReplaceAllString(line, "")
				if resep != nil {
					toks := resep.Split(line, -1)
					ch <- &tmplArgs{Xs: toks, Lines: []string{line}}
				} else {
					lines[k] = line
					k++
				}
			} else {
				if err == io.EOF {
					break
				}
				log.Fatal(err)
			}
			if k == n {
				k = 0
				ch <- &tmplArgs{Lines: lines, Xs: lines}
				lines = make([]string, n)
			}
		}
		if k > 0 {
			ch <- &tmplArgs{Lines: lines[:k], Xs: lines}
		}
		close(ch)
	}()
	return ch
}

type lockWriter struct {
	*bufio.Writer
	mu *sync.Mutex
}

func run(args Params, shell string) {

	stdout := lockWriter{bufio.NewWriter(os.Stdout),
		&sync.Mutex{},
	}
	defer stdout.Flush()

	chXargs := genTmplArgs(args.Nlines, args.Sep)
	cmd := makeCommand(args.Command)
	var wg sync.WaitGroup
	wg.Add(args.Procs)

	for i := 0; i < args.Procs; i++ {
		go func() {
			defer wg.Done()
			for x := range chXargs {
				process(stdout, cmd, &args, x, shell)
			}
		}()
	}

	wg.Wait()
}

func makeCommand(cmd string) string {
	v := strings.Replace(cmd, "{}", "{{index .Lines 0}}", -1)
	re := regexp.MustCompile(`({\d+})`)
	v = re.ReplaceAllStringFunc(v, func(match string) string {
		return "{{index .Xs " + match[1:len(match)-1] + "}}"
	})
	return v
}

func process(stdout lockWriter, cmdStr string, args *Params, tArgs *tmplArgs, shell string) {

	tmpl, err := template.New(cmdStr).Parse(cmdStr)
	check(err)

	var buf bytes.Buffer
	check(tmpl.Execute(&buf, tArgs))

	cmdStr = buf.String()

	if args.Verbose {
		fmt.Fprintf(os.Stderr, "command: %s\n", cmdStr)
	}
	if args.DryRun {
		stdout.mu.Lock()
		fmt.Fprintf(os.Stdout, "%s\n", cmdStr)
		stdout.mu.Unlock()
		return
	}

	cmd := exec.Command(shell, "-c", cmdStr)
	cmd.Stderr = os.Stderr
	pipe, err := cmd.StdoutPipe()
	log.Println("err 0:", err)
	var bPipe *bufio.Reader
	if err == nil {
		bPipe = bufio.NewReader(pipe)
		err = cmd.Start()
	}
	log.Println("err 1:", err)

	if err == nil {
		// try to read 4MB. If we get it all then we get ErrBufferFull
		// will this always be limited by size of buffer?
		res, pErr := bPipe.Peek(4194304)
		log.Println("pErr:", pErr)
		if pErr == bufio.ErrBufferFull || pErr == io.EOF {
			stdout.mu.Lock()
			defer stdout.mu.Unlock()
			var n int
			n, err = stdout.Write(res)
			log.Println("n, err 2:", n, err)
		} else { // otherwise, we use temporary files.
			// TODO: these sometimes get left if process is interrupted.
			// see how it's done in gsort in init() by adding common suffix.
			tmp, xerr := ioutil.TempFile("", "gargsTmp.")
			defer os.Remove(tmp.Name())
			check(xerr)
			bTmp := bufio.NewWriter(tmp)
			_, err = io.Copy(bTmp, bPipe)
			defer tmp.Close()
			if err == nil {
				if err == nil {
					tmp.Seek(0, 0)
					cTmp := bufio.NewReader(tmp)
					stdout.mu.Lock()
					defer stdout.mu.Unlock()
					_, err = io.Copy(stdout, cTmp)
				}

			}
		}
	}
	if err == nil {
		err = cmd.Wait()
	} else {
		cmd.Wait()
	}
	if err != nil {
		var argString string
		if tArgs.Xs != nil && len(tArgs.Xs) > 0 {
			argString = strings.Join(tArgs.Xs, ",")
		} else {
			argString = strings.Join(tArgs.Lines, "|")
		}
		fmt.Fprintf(os.Stderr, "[===\nERROR in command: %s using args: %s\n%s\n===]\n", cmdStr, argString, err)
		ex := err.(*exec.ExitError)
		if st, ok := ex.Sys().(syscall.WaitStatus); ok {
			if !args.ContinueOnError {
				os.Exit(st.ExitStatus())
			} else if st.ExitStatus() > ExitCode {
				ExitCode = st.ExitStatus()
			}
		}
	}
}

package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/orivej/e"
	"golang.org/x/sys/unix"
	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

const R = 0
const W = 1

var importpath = "github.com/orivej/fptrace"
var tracee = "_fptracee"

var wstatusText = map[int]string{
	syscall.PTRACE_EVENT_FORK:       "fork",
	syscall.PTRACE_EVENT_VFORK:      "vfork",
	syscall.PTRACE_EVENT_VFORK_DONE: "vforke",
	syscall.PTRACE_EVENT_CLONE:      "clone",
}

var (
	flEnv      = flag.Bool("e", false, "record environment variables")
	flUndelete = flag.Bool("u", false, "undelete files")
)

var withSeccomp, oldSeccomp bool

var vercmp = collate.New(language.English, collate.Numeric)

func main() {
	flTrace := flag.String("t", "/dev/null", "trace output `file`")
	flTracee := flag.String("tracee", tracee, "tracee `command`")
	flDeps := flag.String("d", "", "deps output `file`")
	flDepsWithOutput := flag.Bool("do", false, "output deps with outputs")
	flDepsWithExec := StringSliceSetFlag("dn", "output deps of `command`(s)")
	flScripts := flag.String("s", "", "scripts output `dir`")
	flRm := flag.Bool("rm", false, "clean up scripts output dir")
	flSeccomp := flag.Bool("seccomp", true, "trace with seccomp (if kernel >= 3.5)")
	flKernel := flag.String("kernel", kernelRelease(), "kernel release (for seccomp)")
	flag.Parse()
	e.Output = os.Stderr
	withSeccomp = *flSeccomp && vercmp.CompareString(*flKernel, "3.5") >= 0
	oldSeccomp = vercmp.CompareString(*flKernel, "4.8") < 0

	args := flag.Args()
	runtime.LockOSThread()
	tracee, err := lookBesideExecutable(*flTracee)
	if err != nil {
		tracee, err = exec.LookPath(*flTracee)
	}
	if err != nil {
		err = fmt.Errorf("%s\ntry running 'go generate %s'", err, importpath)
	}
	e.Exit(err)
	pid, err := trace(tracee, args)
	e.Exit(err)

	f, err := os.Create(*flTrace)
	e.Exit(err)
	defer e.CloseOrPrint(f)
	os.Stdout = f

	sys := NewSysState()
	cmdFDs := map[int]map[int32]string{}
	records := []Record{}

	onExec := func(p *ProcState) {
		fds := map[int32]string{}
		for fd, inode := range p.FDs {
			if inode != 0 {
				fds[fd] = sys.FS.Path(inode)
			}
		}
		cmdFDs[p.CurCmd.ID] = fds

	}
	if *flScripts != "" {
		if *flRm {
			err := os.RemoveAll(*flScripts)
			e.Exit(err)
		}
		err := os.MkdirAll(*flScripts, os.ModePerm)
		e.Exit(err)
		onExec0 := onExec
		onExec = func(p *ProcState) {
			onExec0(p)
			writeScript(*flScripts, *p.CurCmd)
		}
	}

	needOutput := *flDepsWithOutput
	execs := flDepsWithExec.Has
	needExec := len(execs) > 0
	onExit := func(p *ProcState) {
		r := p.Record(sys)
		n := len(r.Outputs)
		if (needOutput || needExec) &&
			(!needOutput || n == 0 || n == 1 && r.Outputs[0] == "/dev/tty") &&
			(!needExec || !execs[r.Cmd.Path] && !execs[filepath.Base(r.Cmd.Path)]) {
			return
		}
		r.FDs = cmdFDs[p.CurCmd.ID]
		delete(cmdFDs, p.CurCmd.ID)
		records = append(records, r)
	}

	rc := mainLoop(sys, pid, onExec, onExit)

	if *flDeps != "" {
		f, err := os.Create(*flDeps)
		e.Exit(err)
		defer e.CloseOrPrint(f)
		err = json.NewEncoder(f).Encode(records)
		e.Exit(err)
	}

	if rc < 0 {
		os.Exit(128 - rc) // Signum + 128 on death by signal.
	}
	os.Exit(rc)
}

func mainLoop(sys *SysState, mainPID int, onExec func(*ProcState), onExit func(*ProcState)) int {
	var err error
	pstates := map[int]*ProcState{}
	mainRC := 0

	p := NewProcState()
	p.CurDir, err = os.Getwd()
	e.Exit(err)
	p.FDs[0] = sys.FS.Inode("/dev/stdin")
	p.FDs[1] = sys.FS.Inode("/dev/stdout")
	p.FDs[2] = sys.FS.Inode("/dev/stderr")
	pstates[mainPID] = p

	suspended := map[int]int{}
	terminated := map[int]bool{}
	running := map[int]bool{mainPID: true}
	pidcmds := map[int]*Cmd{} // Map main PID of each execution to its Cmd.
	term := func(pid int) {
		if !terminated[pid] {
			terminate(pid, pstates[pid], onExit)
			terminated[pid] = true
			delete(running, pid)
		}
	}
	for {
		pid, trapCause, ok := waitForSyscall()
		if !ok {
			if cmd := pidcmds[pid]; cmd != nil {
				cmd.Exit = trapCause
			}
			term(pid)
			delete(pidcmds, pid)
			if mainPID == pid {
				mainPID, mainRC = 0, trapCause // Preserve exit status.
			}
			if len(running) == 0 {
				return mainRC // Exit with the last process.
			}
			continue
		}

		// Select PID state.
		pstate, ok := pstates[pid]
		if !ok {
			// Keep this PID suspended until we are notified of its creation.
			suspended[pid] = trapCause
			fmt.Println(pid, "_suspend")
			continue
		}

	wstatusSwitch:
		switch trapCause {
		case syscall.PTRACE_EVENT_FORK,
			syscall.PTRACE_EVENT_VFORK,
			syscall.PTRACE_EVENT_VFORK_DONE,
			syscall.PTRACE_EVENT_CLONE:
			// New proc.
			unewpid, err := syscall.PtraceGetEventMsg(pid)
			e.Exit(err)
			newpid := int(unewpid)
			cloneFiles := false
			if trapCause == syscall.PTRACE_EVENT_CLONE {
				regs, ok := getRegs(pid)
				cloneFiles = ok && regs.Rdx&syscall.CLONE_FILES != 0
			}
			pstates[newpid] = pstate.Clone(cloneFiles)
			running[newpid] = true
			delete(terminated, newpid)
			fmt.Println(pid, wstatusText[trapCause], newpid)
			// Resume suspended.
			if newstatus, ok := suspended[newpid]; ok {
				delete(suspended, newpid)
				resume(pid, 0, pstate.SysEnter)
				fmt.Println(newpid, "_resume")
				pid, trapCause, pstate = newpid, newstatus, pstates[newpid]
				goto wstatusSwitch
			}
		case syscall.PTRACE_EVENT_EXEC:
			if cmd := pidcmds[pid]; cmd != nil {
				cmd.Exec = sys.Proc.NextID()
			}
			term(pid)
			uoldpid, err := syscall.PtraceGetEventMsg(pid)
			e.Exit(err)
			oldpid := int(uoldpid)
			if oldpid != pid && pstate.IOs.Cnt != 1 {
				panic("lost pstate")
			}
			pstate = pstates[oldpid]
			term(oldpid)
			delete(terminated, pid)
			sys.Proc.Exec(pstate)
			pidcmds[pid] = pstate.CurCmd
			onExec(pstate)
			pstate.SysEnter = true
			pstates[pid] = pstate
			running[pid] = true
			fmt.Println(oldpid, "_exec", pid)
		case unix.PTRACE_EVENT_SECCOMP:
			if pstate.SysEnter {
				panic("seccomp trace event during syscall")
			}
			if oldSeccomp {
				resume(pid, 0, true)
				continue
			}
			fallthrough
		case 0:
			// Toggle edge.
			pstate.SysEnter = !pstate.SysEnter

			var ok bool
			if pstate.SysEnter {
				ok = sysenter(pid, pstate, sys)
			} else {
				ok = sysexit(pid, pstate, sys)
			}

			if !ok {
				term(pid)
				fmt.Println(pid, "_vanish")
				continue
			}
		default:
			panic("unexpected trap cause")
		}
		resume(pid, 0, pstate.SysEnter)
	}
}

func terminate(pid int, pstate *ProcState, onExit func(p *ProcState)) {
	if pstate.IOs.Cnt == 1 && pstate.CurCmd != nil {
		onExit(pstate)
		fmt.Println(pid, "record", *pstate.CurCmd)
	}
	pstate.ResetIOs()
}

func sysenter(pid int, pstate *ProcState, sys *SysState) bool {
	regs, ok := getRegs(pid)
	if !ok {
		return false
	}
	pstate.Syscall = int(regs.Orig_rax)
	switch pstate.Syscall {
	case syscall.SYS_EXECVE:
		pstate.NextCmd = Cmd{
			Path: pstate.Abs(readString(pid, regs.Rdi)),
			Args: readStrings(pid, regs.Rsi),
			Dir:  pstate.CurDir,
		}
		if *flEnv {
			pstate.NextCmd.Env = readStrings(pid, regs.Rdx)
		}
		fmt.Println(pid, "execve", pstate.NextCmd)
	case unix.SYS_EXECVEAT:
		pstate.NextCmd = Cmd{
			Path: absAt(int32(regs.Rdi), readString(pid, regs.Rsi), pid, pstate, sys),
			Args: readStrings(pid, regs.Rdx),
			Dir:  pstate.CurDir,
		}
		if *flEnv {
			pstate.NextCmd.Env = readStrings(pid, regs.R10)
		}
		fmt.Println(pid, "execveat", pstate.NextCmd)
	case syscall.SYS_UNLINK, syscall.SYS_RMDIR:
		if *flUndelete {
			regs.Orig_rax = syscall.SYS_ACCESS
			regs.Rsi = syscall.F_OK
			err := syscall.PtraceSetRegs(pid, &regs)
			e.Exit(err)
		}
	case syscall.SYS_UNLINKAT:
		if *flUndelete {
			regs.Orig_rax = syscall.SYS_FACCESSAT
			regs.R10 = regs.Rdx
			regs.Rdx = syscall.F_OK
			err := syscall.PtraceSetRegs(pid, &regs)
			e.Exit(err)
		}
	}
	return true
}

func sysexit(pid int, pstate *ProcState, sys *SysState) bool {
	regs, ok := getRegs(pid)
	if !ok {
		return false
	}
	ret := int(regs.Rax)
	if ret < 0 {
		return true
	}
	ret32 := int32(ret)
	if pstate.Syscall == syscall.SYS_FCNTL {
		switch regs.Rsi {
		case syscall.F_DUPFD:
			pstate.Syscall = syscall.SYS_DUP
		case syscall.F_DUPFD_CLOEXEC:
			pstate.Syscall = syscall.SYS_DUP3
			regs.Rdx = syscall.O_CLOEXEC
		case syscall.F_SETFD:
			b := regs.Rdx&syscall.FD_CLOEXEC != 0
			pstate.FDCX[int32(regs.Rdi)] = b
			fmt.Println(pid, "fcntl/setfd", regs.Rdi, b)
		}
	}
	switch pstate.Syscall {
	case syscall.SYS_OPEN, syscall.SYS_OPENAT:
		call, at, name, flags := "open", int32(unix.AT_FDCWD), regs.Rdi, regs.Rsi
		if pstate.Syscall == syscall.SYS_OPENAT {
			call, at, name, flags = "openat", int32(regs.Rdi), regs.Rsi, regs.Rdx
		}
		var path string
		switch {
		default:
			path = absAt(at, readString(pid, name), pid, pstate, sys)
		case flags&unix.O_TMPFILE == unix.O_TMPFILE: // It implies O_DIRECTORY.
			path = fmt.Sprintf("/proc/%d/fd/%d", pid, ret32)
		}
		write := flags & (syscall.O_WRONLY | syscall.O_RDWR)
		if write != 0 {
			write = W
		}
		inode := sys.FS.Inode(path)
		pstate.FDs[ret32] = inode
		if flags&syscall.O_CLOEXEC != 0 {
			pstate.FDCX[ret32] = true
		}
		fmt.Println(pid, call, write, path)
		if pstate.IOs.Map[W].Has[inode] {
			break // Treat reads after writes as writes only.
		}
		if !strings.HasPrefix(path, "/dev/fptrace/pipe/") {
			fi, err := os.Lstat(path)
			e.Exit(err)
			if fi.IsDir() {
				break // Do not record directories.
			}
		}
		pstate.IOs.Map[write].Add(inode)
	case syscall.SYS_CHDIR:
		path := pstate.Abs(readString(pid, regs.Rdi))
		pstate.CurDir = path
		fmt.Println(pid, "chdir", path)
	case syscall.SYS_FCHDIR:
		path := sys.FS.Path(pstate.FDs[int32(regs.Rdi)])
		pstate.CurDir = path
		fmt.Println(pid, "fchdir", path)
	case syscall.SYS_LINK:
		oldpath := pstate.Abs(readString(pid, regs.Rdi))
		newpath := pstate.Abs(readString(pid, regs.Rsi))
		oldnode := sys.FS.Inode(oldpath)
		if !pstate.IOs.Map[W].Has[oldnode] {
			pstate.IOs.Map[R].Add(oldnode)
		}
		pstate.IOs.Map[W].Add(sys.FS.Inode(newpath))
		fmt.Println(pid, "link", oldpath, newpath)
	case syscall.SYS_LINKAT:
		oldpath := absAt(int32(regs.Rdi), readString(pid, regs.Rsi), pid, pstate, sys)
		newpath := absAt(int32(regs.Rdx), readString(pid, regs.R10), pid, pstate, sys)
		oldnode := sys.FS.Inode(oldpath)
		if !pstate.IOs.Map[W].Has[oldnode] {
			pstate.IOs.Map[R].Add(oldnode)
		}
		pstate.IOs.Map[W].Add(sys.FS.Inode(newpath))
		fmt.Println(pid, "linkat", oldpath, newpath)
	case syscall.SYS_RENAME:
		oldpath := pstate.Abs(readString(pid, regs.Rdi))
		newpath := pstate.Abs(readString(pid, regs.Rsi))
		sys.FS.Rename(oldpath, newpath)
		fmt.Println(pid, "rename", oldpath, newpath)
	case syscall.SYS_RENAMEAT, unix.SYS_RENAMEAT2:
		oldpath := absAt(int32(regs.Rdi), readString(pid, regs.Rsi), pid, pstate, sys)
		newpath := absAt(int32(regs.Rdx), readString(pid, regs.R10), pid, pstate, sys)
		sys.FS.Rename(oldpath, newpath)
		fmt.Println(pid, "renameat", oldpath, newpath)
	case syscall.SYS_DUP, syscall.SYS_DUP2, syscall.SYS_DUP3:
		pstate.FDs[ret32] = pstate.FDs[int32(regs.Rdi)]
		if pstate.Syscall == syscall.SYS_DUP3 && regs.Rdx&syscall.O_CLOEXEC != 0 {
			pstate.FDCX[ret32] = true
		}
		fmt.Println(pid, "dup", regs.Rdi, ret32, pstate.FDCX[ret32])
	case syscall.SYS_READ, syscall.SYS_PREAD64, syscall.SYS_READV, syscall.SYS_PREADV, unix.SYS_PREADV2:
		inode := pstate.FDs[int32(regs.Rdi)]
		if inode != 0 && !pstate.IOs.Map[W].Has[inode] {
			pstate.IOs.Map[R].Add(inode)
		}
	case syscall.SYS_WRITE, syscall.SYS_PWRITE64, syscall.SYS_WRITEV, syscall.SYS_PWRITEV, unix.SYS_PWRITEV2:
		inode := pstate.FDs[int32(regs.Rdi)]
		if inode != 0 {
			pstate.IOs.Map[W].Add(inode)
		}
	case syscall.SYS_CLOSE:
		n := int32(regs.Rdi)
		pstate.FDs[n] = 0
		delete(pstate.FDCX, n)
		fmt.Println(pid, "close", regs.Rdi)
	case syscall.SYS_PIPE:
		var buf [8]byte
		_, err := syscall.PtracePeekData(pid, uintptr(regs.Rdi), buf[:])
		e.Exit(err)
		readfd := int32(binary.LittleEndian.Uint32(buf[:4]))
		writefd := int32(binary.LittleEndian.Uint32(buf[4:]))
		inode := sys.FS.Pipe()
		pstate.FDs[readfd], pstate.FDs[writefd] = inode, inode
		if regs.Rsi&syscall.O_CLOEXEC != 0 {
			pstate.FDCX[readfd], pstate.FDCX[writefd] = true, true
		}
		fmt.Println(pid, "pipe", readfd, writefd, pstate.FDCX[readfd])
	}
	return true
}

func absAt(dirfd int32, path string, pid int, pstate *ProcState, sys *SysState) string {
	switch {
	case dirfd == unix.AT_FDCWD:
		path = pstate.Abs(path)
	case path == "": // AT_EMPTY_PATH
		path = sys.FS.Path(pstate.FDs[dirfd])
	default:
		path = pstate.AbsAt(sys.FS.Path(pstate.FDs[dirfd]), path)
	}

	// Resolve process-relative paths.
	if strings.HasPrefix(path, "/dev/fd/") {
		path = "/proc/self/fd/" + path[len("/dev/fd/"):]
	}
	if strings.HasPrefix(path, "/proc/self/") {
		var fd int32
		if _, err := fmt.Sscanf(path, "/proc/self/fd/%d", &fd); err == nil {
			if inode, ok := pstate.FDs[fd]; ok {
				return sys.FS.Path(inode)
			}
		}
		return strings.Replace(path, "self", strconv.Itoa(pid), 1)
	}
	return path
}

func kernelRelease() string {
	var uname syscall.Utsname
	err := syscall.Uname(&uname)
	e.Exit(err)
	b := []byte{}
	for _, c := range uname.Release {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

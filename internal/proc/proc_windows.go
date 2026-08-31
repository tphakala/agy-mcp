package proc

import (
	"errors"
	"log"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Supported reports whether process supervision runs on this OS. On Windows the
// process tree is killed via a Job Object rather than a process-group signal.
const Supported = true

func ensureSysProcAttr(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
}

// ConfigureGroup marks the spawned process as the root of a new process group so
// it is isolated from the parent's console control events. The supervisor uses it
// for agy; the process tree is actually terminated via the Job Object captured by
// Track. It ORs the flags into any CreationFlags a caller set first.
//
// CREATE_NO_WINDOW additionally runs the child without a console. The supervisor
// is itself started with DETACHED_PROCESS and so has no console to hand down, and
// a console-mode child that cannot inherit one has a fresh console allocated for
// it, which is a visible window. Suppressing it costs nothing: agy's stdio are
// redirected to a devnull/pipe/file trio, and its tree is killed through the Job
// Object rather than console control events. The flag stays effective alongside
// CREATE_NEW_PROCESS_GROUP; it is ignored only for a non-console application or
// with CREATE_NEW_CONSOLE or DETACHED_PROCESS, none of which apply here.
func ConfigureGroup(cmd *exec.Cmd) {
	ensureSysProcAttr(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW
}

// ConfigureNoWindow suppresses the console window of a short-lived child without
// otherwise changing how it is spawned. It is for the short-lived probes the
// manager runs directly, which want neither a new process group nor Job Object
// supervision, only the window suppression.
//
// A console-mode child that cannot inherit a console gets a fresh one allocated,
// and that console has a visible window, so a manager with no console of its own
// would otherwise flash one window per probe.
func ConfigureNoWindow(cmd *exec.Cmd) {
	ensureSysProcAttr(cmd)
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}

// StartDetached configures cmd so the spawned supervisor is detached from the
// manager and starts it. DETACHED_PROCESS drops the console (the manager's stdio
// is the JSON-RPC stream), and, when the manager sits in a job that permits it,
// CREATE_BREAKAWAY_FROM_JOB frees the supervisor from that job so it outlives the
// manager. The breakaway flag would make CreateProcess fail if the parent job
// forbids it, so it is added only after confirming the job allows breakaway.
func StartDetached(cmd *exec.Cmd) error {
	ensureSysProcAttr(cmd)
	flags := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	if canBreakaway() {
		flags |= windows.CREATE_BREAKAWAY_FROM_JOB
	}
	cmd.SysProcAttr.CreationFlags |= flags
	return cmd.Start()
}

// Group is a handle to a spawned process tree that can be terminated as a unit.
// On Windows it is a Job Object; if the job could not be created or the process
// could not be assigned to it, job is 0 and Terminate falls back to terminating
// the single leader process by pid.
type Group struct {
	job windows.Handle
	pid int
}

// Track puts the already-started process into a fresh Job Object so its whole
// tree can later be terminated together. cmd.Start must have succeeded. Job
// creation or assignment failures are non-fatal: Track logs and returns a
// pid-only Group whose Terminate degrades to single-process termination, so a
// job-object hiccup never leaves the caller unable to stop the job at all.
//
// killOnClose sets JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE so the tracked tree is
// terminated when the last handle (this Group's, closed by Close) goes away. The
// supervisor passes true so agy and its descendants die if the supervisor itself
// dies unexpectedly (a crash or a force-kill), rather than orphaning MCP-server
// grandchildren; this is stricter than Linux, where a crashing supervisor's agy
// is merely reparented. The manager passes false: the supervisor must outlive the
// manager's exit, so closing the manager's handle must NOT kill it.
//
// The process is assigned right after Start, so a grandchild spawned by the
// child in the microseconds before assignment could escape the job. For the agy
// child (which spends its startup initializing before spawning anything) this
// window is not a practical concern.
func Track(cmd *exec.Cmd, killOnClose bool) (*Group, error) {
	if cmd.Process == nil {
		return nil, errors.New("proc: Track before Start")
	}
	pid := cmd.Process.Pid
	g := &Group{pid: pid}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		log.Printf("proc: CreateJobObject for pid %d failed, using single-process termination: %v", pid, err)
		return g, nil
	}
	// Allow a grandchild to break away if it needs to. KILL_ON_JOB_CLOSE is added
	// only when the caller wants the tree torn down with the handle (see above).
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if killOnClose {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		// Non-fatal: TerminateJobObject still kills the members without the flag.
		log.Printf("proc: SetInformationJobObject for pid %d: %v", pid, err)
	}

	// AssignProcessToJobObject needs PROCESS_SET_QUOTA and PROCESS_TERMINATE.
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		log.Printf("proc: OpenProcess for job assignment (pid %d) failed, using single-process termination: %v", pid, err)
		_ = windows.CloseHandle(job)
		return g, nil
	}
	defer func() { _ = windows.CloseHandle(ph) }()
	if err := windows.AssignProcessToJobObject(job, ph); err != nil {
		log.Printf("proc: AssignProcessToJobObject (pid %d) failed, using single-process termination: %v", pid, err)
		_ = windows.CloseHandle(job)
		return g, nil
	}
	g.job = job
	return g, nil
}

// Terminate kills the whole job (the tracked process and every descendant still
// in the job) in one call. sig is ignored: TerminateJobObject is a hard kill,
// so Windows has no SIGTERM grace window (the supervisor calls Terminate twice,
// SIGTERM then SIGKILL; the first already killed everything, so the second is a
// harmless no-op on an empty job). Without a job it terminates the leader pid.
func (g *Group) Terminate(_ syscall.Signal) error {
	if g == nil {
		return syscall.EINVAL
	}
	if g.job != 0 {
		return windows.TerminateJobObject(g.job, 1)
	}
	if g.pid <= 0 {
		return syscall.EINVAL
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(g.pid))
	if err != nil {
		// A non-existent PID (already exited) reports ERROR_INVALID_PARAMETER; treat
		// that as success, mirroring the Linux ESRCH tolerance. Any other failure
		// (e.g. access denied) is a real problem and must surface.
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil
		}
		return err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return windows.TerminateProcess(h, 1)
}

// Close releases the Job Object handle. Because the job has no kill-on-close
// limit, closing it does not terminate the tracked process, so the supervisor
// survives the manager exit that triggers this Close.
func (g *Group) Close() error {
	if g == nil || g.job == 0 {
		return nil
	}
	return windows.CloseHandle(g.job)
}

var procIsProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

// canBreakaway reports whether it is safe to pass CREATE_BREAKAWAY_FROM_JOB when
// spawning the supervisor: true when this process is in no job (the flag is then
// ignored) or in a job that explicitly permits breakaway, and false when it is in
// a job that forbids it (where the flag would make CreateProcess fail). It fails
// safe toward not setting the flag when the job cannot be inspected. x/sys/windows
// does not wrap IsProcessInJob, so that one call is made through a lazy proc.
func canBreakaway() bool {
	var inJob int32
	r1, _, _ := procIsProcessInJob.Call(uintptr(windows.CurrentProcess()), 0, uintptr(unsafe.Pointer(&inJob)))
	if r1 == 0 {
		// IsProcessInJob itself failed (r1 == 0 is the API failure return, not the
		// in-a-job result). Fail closed: if we are in a job that forbids breakaway,
		// setting the flag would make CreateProcess fail and the spawn would fail
		// entirely, which is worse than a supervisor that stays in the parent job.
		return false
	}
	if inJob == 0 {
		return true // not in any job: CREATE_BREAKAWAY_FROM_JOB is ignored
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var ret uint32
	if err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), &ret); err != nil {
		return false // in a job we cannot inspect: do not risk a failed spawn
	}
	// SILENT_BREAKAWAY_OK makes children leave the job automatically without the
	// flag, so only BREAKAWAY_OK requires (and permits) setting it.
	return info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK != 0
}

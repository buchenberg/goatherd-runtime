//go:build windows

package winacl

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The runner's argv sentinels.
//
// Go has no "python -m module" entry point, so the confinement is applied by
// re-executing the host binary with a sentinel argv — the same shape the
// Linux member uses for its trampoline. A host that spawns confined
// processes must call MaybeRunRunner first in its main function; a binary
// that omits the hook makes every launch fail with the confine-failed code,
// which the probe turns into ErrPaddockNotEstablished — fail-closed, never
// silently unconfined.
const (
	// RunnerSentinel marks a confined execution:
	// [exe, RunnerSentinel, <profile>, <command>, <args>...]
	RunnerSentinel = "__winacl_runner__"
	// ProbeSentinel marks a confinement probe:
	// [exe, ProbeSentinel, <profile>, <target>]
	ProbeSentinel = "__winacl_probe__"
	// ProbeChildSentinel is the in-child verb that attempts one file effect:
	// [exe, ProbeChildSentinel, <op>, <target>]
	ProbeChildSentinel = "__winacl_probe_child__"
)

// ConfineFailedRC is the runner's exit code when the restriction could not be
// established.
//
// It is distinct from every code the probe child produces (0 allowed, 1
// denied), so "confinement failed" can never be misread as "write denied" —
// the distinction the fail-closed probe rests on. A broken restriction must
// not be able to pass as a working one.
const ConfineFailedRC = 3

// envProbeVariable is the environment variable the probe child reads when it
// is asked to report the environment it was given.
const envProbeVariable = "SHEPHERD_WINACL_TEST_VALUE"

// MaybeRunRunner handles the runner's argv.
//
// It returns the process exit code and whether it handled the invocation. A
// host calls it as the first statement of its main function:
//
//	func main() {
//		if code, handled := winacl.MaybeRunRunner(os.Args); handled {
//			os.Exit(code)
//		}
//		// ... the binary's real work
//	}
func MaybeRunRunner(args []string) (int, bool) {
	if len(args) < 2 {
		return 0, false
	}
	switch args[1] {
	case RunnerSentinel:
		return runConfined(args[2:], false), true
	case ProbeSentinel:
		return runConfined(args[2:], true), true
	case ProbeChildSentinel:
		return probeChild(args[2:]), true
	}
	return 0, false
}

// runConfined establishes the restriction and runs the child under it.
//
// The order matters: the profile is validated, the grants are applied, the
// token is minted, and only then is the child spawned. Any failure before
// the spawn returns the confine-failed code, so the child never runs
// unconfined.
func runConfined(rest []string, probe bool) int {
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "winacl-run: no profile")
		return ConfineFailedRC
	}
	prof, err := decodeProfile(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run:", err)
		return ConfineFailedRC
	}
	if err := prof.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run:", err)
		return ConfineFailedRC
	}

	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run: build the world SID:", err)
		return ConfineFailedRC
	}
	lowSID, err := windows.CreateWellKnownSid(windows.WinLowLabelSid)
	if err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run: build the low integrity SID:", err)
		return ConfineFailedRC
	}
	writeSIDs := make([]*windows.SID, 0, len(prof.SIDs))
	for _, sidString := range prof.SIDs {
		sid, err := windows.StringToSid(sidString)
		if err != nil {
			fmt.Fprintf(os.Stderr, "winacl-run: parse the write SID %q: %v\n", sidString, err)
			return ConfineFailedRC
		}
		writeSIDs = append(writeSIDs, sid)
	}

	// Apply the grants before the token is minted: the token's restricting SIDs
	// are only meaningful once the ACEs that carry them stand.
	for i, root := range prof.Roots {
		if err := GrantWrite(root, writeSIDs[i], worldSID, lowSID); err != nil {
			fmt.Fprintln(os.Stderr, "winacl-run: grant:", err)
			return ConfineFailedRC
		}
	}

	current, err := openProcessToken()
	if err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run:", err)
		return ConfineFailedRC
	}
	defer current.Close()

	restricted, err := createRestrictedToken(current, writeSIDs, worldSID)
	if err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run:", err)
		return ConfineFailedRC
	}
	defer restricted.Close()

	if err := lowerIntegrity(restricted, lowSID); err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run:", err)
		return ConfineFailedRC
	}
	// The default DACL names a restricting SID so the objects the child
	// creates pass the write pass-2 check. The workspace write SID is named
	// when the profile has one; Everyone closes the read-only case.
	defaultTrustee := worldSID
	if len(writeSIDs) > 0 {
		defaultTrustee = writeSIDs[0]
	}
	if err := setDefaultDacl(restricted, defaultTrustee); err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run:", err)
		return ConfineFailedRC
	}

	var argv []string
	if probe {
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "winacl-run: no probe target")
			return ConfineFailedRC
		}
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "winacl-run: locate own executable:", err)
			return ConfineFailedRC
		}
		argv = []string{exe, ProbeChildSentinel, "write", rest[1]}
	} else {
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "winacl-run: no command")
			return ConfineFailedRC
		}
		argv = rest[1:]
	}
	return spawnAndWait(restricted, argv)
}

// probeChild attempts one file effect and reports the outcome through its
// exit code: 0 when the effect landed, 1 when it was denied.
//
// It runs under the restricted token, so it exercises the real enforcement
// surface rather than the parent's opinion of it.
func probeChild(rest []string) int {
	if len(rest) < 2 {
		return 2
	}
	op, target := rest[0], rest[1]
	var err error
	switch op {
	case "write":
		err = os.WriteFile(target, []byte("winacl probe\n"), 0o600)
	case "delete":
		err = os.Remove(target)
	case "read":
		_, err = os.ReadFile(target)
	case "mkdir":
		err = os.Mkdir(target, 0o700)
	case "writeenv":
		// Writes the value of the named environment variable into the target
		// file, so a test can prove the environment reached the child.
		err = os.WriteFile(target, []byte(os.Getenv(envProbeVariable)), 0o600)
	case "echo":
		// Writes to standard output, so a test can prove the child's standard
		// output reached the runner.
		_, err = os.Stdout.WriteString("winacl-stdout\n")
	default:
		return 2
	}
	if err != nil {
		return 1
	}
	return 0
}

// spawnAndWait runs argv under the token and returns the child's exit code.
//
// Every failure before the child is running returns ConfineFailedRC, never 1:
// the probe reads 1 as "the write was denied", so an infrastructure failure
// reported as 1 would let a broken paddock pass the probe as if it had
// refused the write.
//
// The child is created SUSPENDED, assigned to the kill-on-close job, and only
// then resumed: a child that runs before the assignment can spawn
// grandchildren that are not in the job, and those outlive the runner.
//
// The runner's standard handles are duplicated as inheritable and handed to
// the child through the startup info. Without that the child gets no standard
// handles at all, and a body whose output matters — every body — is mute.
func spawnAndWait(tok windows.Token, argv []string) int {
	if len(argv) == 0 {
		return ConfineFailedRC
	}
	executable, err := exec.LookPath(argv[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run: resolve the command:", err)
		return ConfineFailedRC
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(argv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run: build the command line:", err)
		return ConfineFailedRC
	}
	application, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run: build the application name:", err)
		return ConfineFailedRC
	}
	environment := environmentBlock()

	stdin, stdout, stderr, releaseStdio, err := inheritableStdio()
	if err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run: prepare the standard handles:", err)
		return ConfineFailedRC
	}
	defer releaseStdio()

	startup := windows.StartupInfo{
		Cb:        uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Flags:     windows.STARTF_USESTDHANDLES,
		StdInput:  stdin,
		StdOutput: stdout,
		StdErr:    stderr,
	}
	process := windows.ProcessInformation{}
	creationFlags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	if err := windows.CreateProcessAsUser(tok, application, commandLine, nil, nil, true,
		creationFlags, environment, nil, &startup, &process); err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run: CreateProcessAsUser:", err)
		return ConfineFailedRC
	}
	defer windows.CloseHandle(process.Thread)
	defer windows.CloseHandle(process.Process)

	// The job is the tree-lifetime backstop. A child that cannot be contained
	// by it does not run: the restriction holds on the child itself, but a
	// tree the job cannot bound is a tree that outlives the runner.
	job, err := createKillOnCloseJob()
	if err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run:", err)
		_ = windows.TerminateProcess(process.Process, ConfineFailedRC)
		return ConfineFailedRC
	}
	defer windows.CloseHandle(job)
	if err := windows.AssignProcessToJobObject(job, process.Process); err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run: assign the child to the kill-on-close job:", err)
		_ = windows.TerminateProcess(process.Process, ConfineFailedRC)
		return ConfineFailedRC
	}
	if _, err := windows.ResumeThread(process.Thread); err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run: resume the child:", err)
		_ = windows.TerminateProcess(process.Process, ConfineFailedRC)
		return ConfineFailedRC
	}

	_, _ = windows.WaitForSingleObject(process.Process, waitTimeoutMillis)
	var code uint32
	if err := windows.GetExitCodeProcess(process.Process, &code); err != nil {
		fmt.Fprintln(os.Stderr, "winacl-run: read the child's exit code:", err)
		return ConfineFailedRC
	}
	return int(code)
}

// inheritableStdio duplicates the runner's standard handles as inheritable
// handles, so the confined child receives them.
//
// The runner's own standard handles are not inheritable: they arrived through
// the parent's handle list, and a handle list exists precisely to keep the
// child's handles from being inherited further. Passing them on therefore
// takes a duplicate marked inheritable.
//
// The child also inherits any other inheritable handle the runner holds. The
// runner is this binary and holds none beyond the standard handles, the
// process and thread handles (which are not inheritable), and the job (also
// not inheritable), so the exposure is the three standard handles.
func inheritableStdio() (stdin, stdout, stderr windows.Handle, release func(), err error) {
	duplicate := func(stdHandle uint32) (windows.Handle, error) {
		source, err := windows.GetStdHandle(stdHandle)
		if err != nil {
			return 0, fmt.Errorf("read the standard handle: %w", err)
		}
		if source == 0 || source == windows.InvalidHandle {
			return 0, fmt.Errorf("the standard handle %d is not set", stdHandle)
		}
		var target windows.Handle
		if err := windows.DuplicateHandle(windows.CurrentProcess(), source,
			windows.CurrentProcess(), &target, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
			return 0, fmt.Errorf("duplicate the standard handle: %w", err)
		}
		return target, nil
	}
	stdin, err = duplicate(windows.STD_INPUT_HANDLE)
	if err != nil {
		return 0, 0, 0, func() {}, err
	}
	stdout, err = duplicate(windows.STD_OUTPUT_HANDLE)
	if err != nil {
		windows.CloseHandle(stdin)
		return 0, 0, 0, func() {}, err
	}
	stderr, err = duplicate(windows.STD_ERROR_HANDLE)
	if err != nil {
		windows.CloseHandle(stdin)
		windows.CloseHandle(stdout)
		return 0, 0, 0, func() {}, err
	}
	return stdin, stdout, stderr, func() {
		windows.CloseHandle(stdin)
		windows.CloseHandle(stdout)
		windows.CloseHandle(stderr)
	}, nil
}

// waitTimeoutMillis bounds the wait for the confined child. A body that
// outlives it is abandoned rather than waited on forever; the kill-on-close
// job reaps the tree when the runner exits.
const waitTimeoutMillis = 24 * 60 * 60 * 1000

// createKillOnCloseJob creates an unnamed job whose members die with it.
func createKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create the kill-on-close job: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("set the kill-on-close job limit: %w", err)
	}
	runtime.KeepAlive(info)
	return job, nil
}

// environmentBlock builds a double-null-terminated UTF-16 environment block.
//
// CreateProcessAsUser does not inherit the caller's environment, so the block
// is built explicitly.
//
// The terminator handling is normalized rather than assumed: the UTF-16
// encoding of a string may or may not carry its own trailing NUL (the x/sys
// helper has changed shape across versions), so any trailing NUL is stripped
// and exactly one is appended per entry. Appending a second would end the
// block after its first variable, and every later entry — including PATH —
// would be dropped.
func environmentBlock() *uint16 {
	block := make([]uint16, 0, 1024)
	for _, entry := range os.Environ() {
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			continue
		}
		for len(encoded) > 0 && encoded[len(encoded)-1] == 0 {
			encoded = encoded[:len(encoded)-1]
		}
		block = append(block, encoded...)
		block = append(block, 0)
	}
	if len(block) == 0 {
		// An empty environment is still a terminated block.
		block = append(block, 0, 0)
	} else {
		block = append(block, 0)
	}
	return &block[0]
}

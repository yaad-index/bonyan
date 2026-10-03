//go:build unix

package agent_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/approval"
	"github.com/yaad-index/bonyan/runstore"
)

// exitAfterSave stops the process the moment its run is saved: nothing the
// run would do after that, dropping its action or removing its state,
// happens.
type exitAfterSave struct{ runstore.Store }

func (e exitAfterSave) Save(ctx context.Context, s runstore.State, token string) error {
	if err := e.Store.Save(ctx, s, token); err != nil {
		return err
	}
	os.Exit(3)
	return nil
}

// exitAfterProceed stops the process the moment its run's decided action is
// marked proceeding, before the action runs.
type exitAfterProceed struct{ runstore.Store }

func (e exitAfterProceed) Proceed(ctx context.Context, run, token string) error {
	if err := e.Store.Proceed(ctx, run, token); err != nil {
		return err
	}
	os.Exit(4)
	return nil
}

// TestHelperCrash is run as its own process by the crash tests: it stops at
// the save, or with AGENT_CRASH_AT=proceed once its action is marked.
func TestHelperCrash(t *testing.T) {
	dir := os.Getenv("AGENT_CRASH_DIR")
	if dir == "" {
		t.Skip("run by TestARunSurvivesACrash")
	}
	runs, err := runstore.OpenDir(filepath.Join(dir, "runs"))
	require.NoError(t, err)
	approvals, err := approval.OpenDir(filepath.Join(dir, "approvals"))
	require.NoError(t, err)
	var store runstore.Store = exitAfterSave{runs}
	if os.Getenv("AGENT_CRASH_AT") == "proceed" {
		store = exitAfterProceed{runs}
		approvals.PollEvery = 5 * time.Millisecond
	}
	p := newProcess(t, store, approvals, twoCalls, answer("never"))
	_, _, err = agent.Run(context.Background(), p.agent, input("find x"))
	require.NoError(t, err)
	require.Fail(t, "the process should have stopped at the save")
}

// A run whose process dies while its action waits is resumed from the
// directory stores by a new process, with a decision made while no process
// waited.
func TestARunSurvivesACrash(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrash$", "-test.count=1")
	cmd.Env = append(os.Environ(), "AGENT_CRASH_DIR="+dir)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit, "%s", out)
	require.Equal(t, 3, exit.ExitCode(), "%s", out)

	runs, err := runstore.OpenDir(filepath.Join(dir, "runs"))
	require.NoError(t, err)
	approvals, err := approval.OpenDir(filepath.Join(dir, "approvals"))
	require.NoError(t, err)
	list, err := runs.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	held, err := approvals.List(context.Background())
	require.NoError(t, err)
	require.Len(t, held, 1)
	require.NoError(t, approvals.Decide(context.Background(), held[0].ID, true))

	id := list[0].Run
	p := newProcess(t, runs, approvals, answer("done"))
	got, rep, err := agent.Resume(context.Background(), p.agent, id)
	require.NoError(t, err)
	a, ok := got.Answer()
	require.True(t, ok, got.String())
	assert.Equal(t, "done", a)
	assert.Equal(t, []string{"search"}, names(p.tools.calls), "the action, once; lookup ran before the crash")
	assert.Equal(t, 2, rep.Steps)
	list, err = runs.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)
	_, _, err = agent.Resume(context.Background(), p.agent, id)
	require.ErrorIs(t, err, agent.ErrUnknownRun, "an ended run is gone")
}

// A process that dies after its action was decided and marked, before or
// while the action runs, leaves a run no new process resumes: the mark is
// on disk, so the action never runs twice.
func TestAMarkedActionSurvivesACrash(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperCrash$", "-test.count=1")
	cmd.Env = append(os.Environ(), "AGENT_CRASH_DIR="+dir, "AGENT_CRASH_AT=proceed")
	require.NoError(t, cmd.Start())
	runs, err := runstore.OpenDir(filepath.Join(dir, "runs"))
	require.NoError(t, err)
	approvals, err := approval.OpenDir(filepath.Join(dir, "approvals"))
	require.NoError(t, err)
	var held []approval.Pending
	require.Eventually(t, func() bool {
		held, _ = approvals.List(context.Background())
		list, _ := runs.List(context.Background())
		return len(held) == 1 && len(list) == 1
	}, 10*time.Second, 5*time.Millisecond)
	require.NoError(t, approvals.Decide(context.Background(), held[0].ID, true))
	err = cmd.Wait()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 4, exit.ExitCode())

	list, err := runs.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	p := newProcess(t, runs, approvals, answer("done"))
	_, _, err = agent.Resume(context.Background(), p.agent, list[0].Run)
	require.ErrorIs(t, err, agent.ErrActionStarted)
	assert.Empty(t, p.tools.calls)
}

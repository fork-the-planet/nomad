// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package taskrunner

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/vaultclient"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers/fsisolation"
	"github.com/shoenig/test/must"
)

func TestTaskRunner_DisableFileForVaultToken_UpgradePath(t *testing.T) {
	ci.Parallel(t)
	ci.SkipTestWithoutRootAccess(t)

	// Create test allocation with a Vault block.
	alloc := mock.BatchAlloc()
	task := alloc.Job.TaskGroups[0].Tasks[0]
	task.Config = map[string]any{
		"run_for": "0s",
	}
	task.Vault = &structs.Vault{
		Cluster: structs.VaultDefaultCluster,
	}

	// Setup a test Vault client.
	token := "1234"
	handler := func(ctx context.Context, req vaultclient.JWTLoginRequest) (string, bool, int, error) {
		return token, true, 30, nil
	}
	vc, err := vaultclient.NewMockVaultClient(structs.VaultDefaultCluster)
	must.NoError(t, err)
	vaultClient := vc.(*vaultclient.MockVaultClient)
	vaultClient.SetDeriveTokenWithJWTFn(handler)

	conf, cleanup := testTaskRunnerConfig(t, alloc, task.Name, vaultClient)
	defer cleanup()

	// Remove private dir and write the Vault token to the secrets dir to
	// simulate an old task.
	err = conf.TaskDir.Build(fsisolation.None, nil, task.User)
	must.NoError(t, err)

	err = syscall.Unmount(conf.TaskDir.PrivateDir, 0)
	must.NoError(t, err)
	err = os.Remove(conf.TaskDir.PrivateDir)
	must.NoError(t, err)

	tokenPath := filepath.Join(conf.TaskDir.SecretsDir, vaultTokenFile)
	err = os.WriteFile(tokenPath, []byte(token), 0666)
	must.NoError(t, err)

	// Start task runner and wait for task to finish.
	tr, err := NewTaskRunner(conf)
	must.NoError(t, err)
	defer tr.Kill(context.Background(), structs.NewTaskEvent("cleanup"))
	go tr.Run()
	time.Sleep(500 * time.Millisecond)

	testWaitForTaskToDie(t, tr)

	// Verify task exited successfully.
	finalState := tr.TaskState()
	must.Eq(t, structs.TaskStateDead, finalState.State)
	must.False(t, finalState.Failed)

	// Verify token is in secrets dir.
	tokenPath = filepath.Join(conf.TaskDir.SecretsDir, vaultTokenFile)
	data, err := os.ReadFile(tokenPath)
	must.NoError(t, err)
	must.Eq(t, token, string(data))

	// Varify token is not in private dir since the allocation doesn't have
	// this path.
	tokenPath = filepath.Join(conf.TaskDir.PrivateDir, vaultTokenFile)
	_, err = os.Stat(tokenPath)
	must.ErrorIs(t, err, os.ErrNotExist)
}

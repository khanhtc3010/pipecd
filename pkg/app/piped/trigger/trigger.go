// Copyright 2021 The PipeCD Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package trigger provides a piped component
// that detects a list of application should be synced (by new commit, sync command or configuration drift)
// and then sends request to the control-plane to create a new Deployment.
package trigger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/pipe-cd/pipe/pkg/app/api/service/pipedservice"
	"github.com/pipe-cd/pipe/pkg/cache/memorycache"
	"github.com/pipe-cd/pipe/pkg/config"
	"github.com/pipe-cd/pipe/pkg/git"
	"github.com/pipe-cd/pipe/pkg/model"
)

const (
	ondemandCheckInterval               = 10 * time.Second
	defaultLastTriggeredCommitCacheSize = 500
	triggeredDeploymentIDKey            = "TriggeredDeploymentID"
)

type apiClient interface {
	GetApplicationMostRecentDeployment(ctx context.Context, req *pipedservice.GetApplicationMostRecentDeploymentRequest, opts ...grpc.CallOption) (*pipedservice.GetApplicationMostRecentDeploymentResponse, error)
	CreateDeployment(ctx context.Context, in *pipedservice.CreateDeploymentRequest, opts ...grpc.CallOption) (*pipedservice.CreateDeploymentResponse, error)
	ReportApplicationMostRecentDeployment(ctx context.Context, req *pipedservice.ReportApplicationMostRecentDeploymentRequest, opts ...grpc.CallOption) (*pipedservice.ReportApplicationMostRecentDeploymentResponse, error)
}

type gitClient interface {
	Clone(ctx context.Context, repoID, remote, branch, destination string) (git.Repo, error)
}

type applicationLister interface {
	Get(id string) (*model.Application, bool)
	List() []*model.Application
}

type commandLister interface {
	ListApplicationCommands() []model.ReportableCommand
}

type environmentLister interface {
	Get(ctx context.Context, id string) (*model.Environment, error)
}

type notifier interface {
	Notify(event model.NotificationEvent)
}

type candidateKind int

const (
	commitCandidate candidateKind = iota
	commandCandidate
	outOfSyncCandidate
)

type candidate struct {
	application *model.Application
	kind        candidateKind
	command     model.ReportableCommand
}

type Trigger struct {
	apiClient         apiClient
	gitClient         gitClient
	applicationLister applicationLister
	commandLister     commandLister
	environmentLister environmentLister
	notifier          notifier
	config            *config.PipedSpec
	commitStore       *lastTriggeredCommitStore
	gitRepos          map[string]git.Repo
	gracePeriod       time.Duration
	logger            *zap.Logger
}

func NewTrigger(
	apiClient apiClient,
	gitClient gitClient,
	appLister applicationLister,
	commandLister commandLister,
	environmentLister environmentLister,
	notifier notifier,
	cfg *config.PipedSpec,
	gracePeriod time.Duration,
	logger *zap.Logger,
) (*Trigger, error) {

	cache, err := memorycache.NewLRUCache(defaultLastTriggeredCommitCacheSize)
	if err != nil {
		return nil, err
	}
	commitStore := &lastTriggeredCommitStore{
		apiClient: apiClient,
		cache:     cache,
	}

	t := &Trigger{
		apiClient:         apiClient,
		gitClient:         gitClient,
		applicationLister: appLister,
		commandLister:     commandLister,
		environmentLister: environmentLister,
		notifier:          notifier,
		config:            cfg,
		commitStore:       commitStore,
		gitRepos:          make(map[string]git.Repo, len(cfg.Repositories)),
		gracePeriod:       gracePeriod,
		logger:            logger.Named("trigger"),
	}

	return t, nil
}

func (t *Trigger) Run(ctx context.Context) error {
	t.logger.Info("start running deployment trigger")

	// Pre cloning to cache the registered git repositories.
	t.gitRepos = make(map[string]git.Repo, len(t.config.Repositories))
	for _, r := range t.config.Repositories {
		repo, err := t.gitClient.Clone(ctx, r.RepoID, r.Remote, r.Branch, "")
		if err != nil {
			t.logger.Error(fmt.Sprintf("failed to clone git repository %s", r.RepoID), zap.Error(err))
			return err
		}
		t.gitRepos[r.RepoID] = repo
	}

	syncTicker := time.NewTicker(time.Duration(t.config.SyncInterval))
	defer syncTicker.Stop()

	ondemandTicker := time.NewTicker(ondemandCheckInterval)
	defer ondemandTicker.Stop()

	for {
		select {
		case <-syncTicker.C:
			var (
				commitCandidates    = t.listCommitCandidates()
				outOfSyncCandidates = t.listOutOfSyncCandidates()
				candidates          = append(commitCandidates, outOfSyncCandidates...)
			)
			t.logger.Info(fmt.Sprintf("found %d candidates: %d commit candidates and %d out_of_sync candidates",
				len(candidates),
				len(commitCandidates),
				len(outOfSyncCandidates),
			))
			t.checkCandidates(ctx, candidates)

		case <-ondemandTicker.C:
			candidates := t.listCommandCandidates()
			t.logger.Info(fmt.Sprintf("found %d command candidates", len(candidates)))
			t.checkCandidates(ctx, candidates)

		case <-ctx.Done():
			t.logger.Info("deployment trigger has been stopped")
			return nil
		}
	}
}

func (t *Trigger) checkCandidates(ctx context.Context, cs []candidate) (err error) {
	// Group candidates by repository to reduce the number of Git operations on each repo.
	csm := make(map[string][]candidate)
	for _, c := range cs {
		repoId := c.application.GitPath.Repo.Id
		if _, ok := csm[repoId]; !ok {
			csm[repoId] = []candidate{c}
			continue
		}
		csm[repoId] = append(csm[repoId], c)
	}

	// Iterate each repository and check its candidates.
	// Only the last error will be returned.
	for repoID, cs := range csm {
		if e := t.checkRepoCandidates(ctx, repoID, cs); e != nil {
			t.logger.Error(fmt.Sprintf("failed while checking applications in repo %s", repoID), zap.Error(e))
			err = e
		}
	}
	return
}

func (t *Trigger) checkRepoCandidates(ctx context.Context, repoID string, cs []candidate) error {
	gitRepo, branch, headCommit, err := t.updateRepoToLatest(ctx, repoID)
	if err != nil {
		// TODO: Find a better way to skip the CANCELLED error log while shutting down.
		if ctx.Err() != context.Canceled {
			t.logger.Error(fmt.Sprintf("failed to update git repository %s to latest", repoID), zap.Error(err))
		}
		return err
	}

	ds := &determiners{
		onCommand:   &OnCommandDeterminer{},
		onOutOfSync: &OnOutOfSyncDeterminer{},
		onCommit:    NewOnCommitDeterminer(gitRepo, headCommit.Hash, t.commitStore, t.logger),
	}

	for _, c := range cs {
		app := c.application

		appCfg, err := loadDeploymentConfiguration(gitRepo.GetPath(), app)
		if err != nil {
			t.logger.Error("failed to load application config file", zap.Error(err))
			// Do not notify this event to external services because it may cause annoying
			// when one application is missing or having an invalid configuration file.
			// So instead of notifying this as a notification,
			// we should show this problem on the web with a status like INVALID_CONFIG.
			//
			// t.notifyDeploymentTriggerFailed(app, msg, headCommit)
			continue
		}

		shouldTrigger, err := ds.Determiner(c.kind).ShouldTrigger(ctx, app, appCfg)
		if err != nil {
			msg := fmt.Sprintf("failed while determining whether application %s should be triggered or not: %s", app.Name, err)
			t.notifyDeploymentTriggerFailed(app, msg, headCommit)
			t.logger.Error(msg, zap.Error(err))
			continue
		}

		if !shouldTrigger {
			t.commitStore.Put(app.Id, headCommit.Hash)
			continue
		}

		// Build deployment model and send a request to API to create a new deployment.
		deployment, err := t.triggerDeployment(ctx, app, appCfg, branch, headCommit, "", model.SyncStrategy_AUTO)
		if err != nil {
			msg := fmt.Sprintf("failed to trigger application %s: %v", app.Id, err)
			t.notifyDeploymentTriggerFailed(app, msg, headCommit)
			t.logger.Error(msg, zap.Error(err))
			continue
		}

		t.commitStore.Put(app.Id, headCommit.Hash)
		t.notifyDeploymentTriggered(ctx, appCfg, deployment)

		// Mask command as handled since the deployment has been triggered successfully.
		if c.kind == commandCandidate {
			metadata := map[string]string{
				triggeredDeploymentIDKey: deployment.Id,
			}
			if err := c.command.Report(ctx, model.CommandStatus_COMMAND_SUCCEEDED, metadata, nil); err != nil {
				t.logger.Error("failed to report command status", zap.Error(err))
			}
		}
	}

	return nil
}

// listCommandCandidates finds all applications that have been commanded to sync.
func (t *Trigger) listCommandCandidates() []candidate {
	var (
		cmds = t.commandLister.ListApplicationCommands()
		apps = make([]candidate, 0)
	)

	for _, cmd := range cmds {
		// Filter out commands that are not SYNC command.
		syncCmd := cmd.GetSyncApplication()
		if syncCmd == nil {
			continue
		}

		// Find the target application specified in command.
		app, ok := t.applicationLister.Get(syncCmd.ApplicationId)
		if !ok {
			t.logger.Warn("detected an AppSync command for an unregistered application",
				zap.String("command", cmd.Id),
				zap.String("app-id", syncCmd.ApplicationId),
				zap.String("commander", cmd.Commander),
			)
			continue
		}

		apps = append(apps, candidate{
			application: app,
			kind:        commandCandidate,
			command:     cmd,
		})
	}

	return apps
}

// listOutOfSyncCandidates finds all applications that are staying at OUT_OF_SYNC state.
func (t *Trigger) listOutOfSyncCandidates() []candidate {
	var (
		list = t.applicationLister.List()
		apps = make([]candidate, 0)
	)
	for _, app := range list {
		if app.SyncState.Status != model.ApplicationSyncStatus_OUT_OF_SYNC {
			continue
		}
		apps = append(apps, candidate{
			application: app,
			kind:        outOfSyncCandidate,
		})
	}
	return apps
}

// listCommitCandidates finds all applications that have potentiality
// to be candidates by the changes of new commits.
// They are all applications managed by this Piped.
func (t *Trigger) listCommitCandidates() []candidate {
	var (
		list = t.applicationLister.List()
		apps = make([]candidate, 0)
	)
	for _, app := range list {
		apps = append(apps, candidate{
			application: app,
			kind:        commitCandidate,
		})
	}
	return apps
}

// updateRepoToLatest ensures that the local data of the given Git repository should be up-to-date.
func (t *Trigger) updateRepoToLatest(ctx context.Context, repoID string) (repo git.Repo, branch string, headCommit git.Commit, err error) {
	var ok bool

	// Find the repository from the previously loaded list.
	repo, ok = t.gitRepos[repoID]
	if !ok {
		err = fmt.Errorf("the repository was not registered in Piped configuration")
		return
	}
	branch = repo.GetClonedBranch()

	// Fetch to update the repository.
	err = repo.Pull(ctx, branch)
	if err != nil {
		return
	}

	// Get the head commit of the repository.
	headCommit, err = repo.GetLatestCommit(ctx)
	return
}

func (t *Trigger) GetLastTriggeredCommitGetter() LastTriggeredCommitGetter {
	return t.commitStore
}

func (t *Trigger) notifyDeploymentTriggered(ctx context.Context, appCfg *config.GenericDeploymentSpec, d *model.Deployment) {
	var mentions []string
	if n := appCfg.DeploymentNotification; n != nil {
		mentions = n.FindSlackAccounts(model.NotificationEventType_EVENT_DEPLOYMENT_TRIGGERED)
	}

	if env, err := t.environmentLister.Get(ctx, d.EnvId); err == nil {
		t.notifier.Notify(model.NotificationEvent{
			Type: model.NotificationEventType_EVENT_DEPLOYMENT_TRIGGERED,
			Metadata: &model.NotificationEventDeploymentTriggered{
				Deployment:        d,
				EnvName:           env.Name,
				MentionedAccounts: mentions,
			},
		})
	}
}

func (t *Trigger) notifyDeploymentTriggerFailed(app *model.Application, reason string, commit git.Commit) {
	t.notifier.Notify(model.NotificationEvent{
		Type: model.NotificationEventType_EVENT_DEPLOYMENT_TRIGGER_FAILED,
		Metadata: &model.NotificationEventDeploymentTriggerFailed{
			Application:   app,
			CommitHash:    commit.Hash,
			CommitMessage: commit.Message,
			Reason:        reason,
		},
	})
}

func loadDeploymentConfiguration(repoPath string, app *model.Application) (*config.GenericDeploymentSpec, error) {
	var (
		relPath = app.GitPath.GetDeploymentConfigFilePath()
		absPath = filepath.Join(repoPath, relPath)
	)

	cfg, err := config.LoadFromYAML(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("application config file %s was not found in Git", relPath)
		}
		return nil, err
	}
	if appKind, ok := config.ToApplicationKind(cfg.Kind); !ok || appKind != app.Kind {
		return nil, fmt.Errorf("invalid application kind in the deployment config file, got: %s, expected: %s", appKind, app.Kind)
	}

	spec, ok := cfg.GetGenericDeployment()
	if !ok {
		return nil, fmt.Errorf("unsupported application kind: %s", app.Kind)
	}

	return &spec, nil
}

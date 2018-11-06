package worker

import (
	"encoding/json"
	"fmt"
	"github.com/choerodon/choerodon-cluster-agent/controller"
	"github.com/choerodon/choerodon-cluster-agent/manager"
	"strings"
	"sync"

	"github.com/golang/glog"

	"time"

	"github.com/choerodon/choerodon-cluster-agent/pkg/cluster"
	"github.com/choerodon/choerodon-cluster-agent/pkg/git"
	"github.com/choerodon/choerodon-cluster-agent/pkg/helm"
	"github.com/choerodon/choerodon-cluster-agent/pkg/kube"
	"github.com/choerodon/choerodon-cluster-agent/pkg/model"
	"github.com/choerodon/choerodon-cluster-agent/ws"
)

var (
	processCmdFuncs = make(map[string]processCmdFunc)
)

type processCmdFunc func(w *workerManager, cmd *model.Packet) ([]*model.Packet, *model.Packet)

type workerManager struct {
	chans              *manager.CRChan
	clusterId          int
	helmClient         helm.Client
	kubeClient         kube.Client
	appClient          ws.WebSocketClient
	agentInitOps       *model.AgentInitOptions
	gitConfig          git.Config
	gitRepos           map[string]*git.Repo
	syncSoon           map[string]chan struct{}
	repoStopChans      map[string]chan struct{}
	syncInterval       time.Duration
	manifests          cluster.Manifests
	cluster            cluster.Cluster
	statusSyncInterval time.Duration
	gitTimeout         time.Duration
	controllerContext  *controller.ControllerContext
	wg                 *sync.WaitGroup
	stop               <-chan struct{}
}

func NewWorkerManager(
	chans *manager.CRChan,
	kubeClient kube.Client,
	helmClient helm.Client,
	appClient ws.WebSocketClient,
	manifests cluster.Manifests,
	cluster cluster.Cluster,
	agentInitOps *model.AgentInitOptions,
	syncInterval time.Duration,
	statusSyncInterval time.Duration,
	gitTimeout time.Duration,
	gitConfig git.Config,
	controllerContext *controller.ControllerContext,
	wg *sync.WaitGroup,
	stop <-chan struct{}) *workerManager {
	return &workerManager{
		chans:              chans,
		helmClient:         helmClient,
		kubeClient:         kubeClient,
		appClient:          appClient,
		agentInitOps:       agentInitOps,
		syncInterval:       syncInterval,
		statusSyncInterval: statusSyncInterval,
		gitTimeout:         gitTimeout,
		gitRepos:           map[string]*git.Repo{},
		gitConfig:          gitConfig,
		syncSoon:           map[string]chan struct{}{},
		repoStopChans:      map[string]chan struct{}{},
		wg:                 wg,
		stop:               stop,
		controllerContext:  controllerContext,
		manifests:          manifests,
		cluster:            cluster,
	}
}

func (w *workerManager) Start() {
	w.wg.Add(1)
	go w.syncStatus(w.stop, w.wg)

	//gitconfigChan := make(chan model.GitInitConfig, 1)

	w.wg.Add(1)
	go w.runWorker()

	//if w.gitConfig.GitUrl == "" {
	//	for {
	//		gitConfig := <-gitconfigChan
	//		gitRemote := git.Remote{URL: gitConfig.GitUrl}
	//		glog.Infof("receive manager git url %s and git ssh key :\n%s", gitConfig.GitUrl, gitConfig.SshKey)
	//		if err := writeSSHkey(gitConfig.SshKey); err != nil {
	//			glog.Errorf("write git ssh key error", err)
	//		} else {
	//			glog.Info("Init Git Config Success")
	//			w.gitRepo = git.NewRepo(gitRemote, git.PollInterval(w.gitConfig.GitPollInterval))
	//
	//			{
	//				wg.Add(1)
	//				go func() {
	//					err := w.gitRepo.Start(stop, wg)
	//					if err != nil {
	//						glog.Errorf("git repo start failed", err)
	//					}
	//				}()
	//			}
	//			break
	//		}
	//	}
	//}

}

func (w *workerManager) runWorker() {
	defer w.wg.Done()
	for {
		select {
		case <-w.stop:
			glog.Infof("worker down!")
			return
		case cmd := <-w.chans.CommandChan:
			go func(cmd *model.Packet) {
				glog.Infof("get command: %s/%s", cmd.Key, cmd.Type)
				var newCmds []*model.Packet = nil
				var resp *model.Packet = nil

				if processCmdFunc, ok := processCmdFuncs[cmd.Type]; !ok {
					err := fmt.Errorf("type %s not exist", cmd.Type)
					glog.V(1).Info(err.Error())
				} else {
					newCmds, resp = processCmdFunc(w, cmd)
				}

				if newCmds != nil {
					go func(newCmds []*model.Packet) {
						for i := 0; i < len(newCmds); i++ {
							w.chans.CommandChan <- newCmds[i]
						}
					}(newCmds)
				}
				if resp != nil {
					go func(resp *model.Packet) {
						w.chans.ResponseChan <- resp
					}(resp)
				}
			}(cmd)
		}
	}
}

func registerCmdFunc(funcType string, f processCmdFunc) {
	processCmdFuncs[funcType] = f
}

func setRepos(w *workerManager, cmd *model.Packet) ([]*model.Packet, *model.Packet) {

	var newAgentInitOps model.AgentInitOptions
	err := json.Unmarshal([]byte(cmd.Payload), &newAgentInitOps)
	if err != nil {
		return nil, NewResponseError(cmd.Key, model.InitAgentFailed, err)
	}

	namespaces := w.controllerContext.Namespaces

	var sshConfig string

	toAddEnv := &model.AgentInitOptions{
		Envs: []model.EnvParas{},
	}
	toAddEnv.GitHost = newAgentInitOps.GitHost

	// 往文件中写入各个git库deploy key
	for _, envPara := range newAgentInitOps.Envs {
		err = writeSSHkey(envPara.Namespace, envPara.GitRsaKey)
		if err != nil {
			return nil, NewResponseError(cmd.Key, model.InitAgentFailed, err)
		}
		sshConfig = sshConfig + config(newAgentInitOps.GitHost, envPara.Namespace)

		if !namespaces.Contain(envPara.Namespace) {
			toAddEnv.Envs = append(toAddEnv.Envs, envPara)
		}

	}

	err = writeSshConfig(sshConfig)
	if err != nil {
		return nil, NewResponseError(cmd.Key, model.InitAgentFailed, err)
	}

	nsList := []string{}
	for _, envPara := range newAgentInitOps.Envs {
		nsList = append(nsList, envPara.Namespace)
	}
	namespaces.Set(nsList)
	w.addEnv(toAddEnv)
	w.removeEnvs(newAgentInitOps)
	w.agentInitOps = &newAgentInitOps
	return nil, &model.Packet{
		Key:     "test:test",
		Type:    model.InitAgentSucceed,
		Payload: cmd.Payload,
	}

}

func (w *workerManager) addEnv(agentInitOps *model.AgentInitOptions) {
	for _, envPara := range agentInitOps.Envs {
		gitRemote := git.Remote{URL: strings.Replace(envPara.GitUrl, agentInitOps.GitHost, envPara.Namespace, 1)}
		repo := git.NewRepo(gitRemote, git.PollInterval(w.gitConfig.GitPollInterval))
		w.wg.Add(1)
		repoStopChan := make(chan struct{}, 1)
		go func() {
			err := repo.Start(w.stop, repoStopChan, w.wg)
			if err != nil {
				glog.Errorf("git repo start failed", err)
			}
		}()
		w.syncSoon[envPara.Namespace] = make(chan struct{}, 1)
		w.gitRepos[envPara.Namespace] = repo
		w.repoStopChans[envPara.Namespace] = repoStopChan
		w.wg.Add(1)
		go w.syncLoop(w.stop, envPara.Namespace, repoStopChan, w.wg)
	}
}

func (w *workerManager) removeEnvs(newOpt model.AgentInitOptions) {
	for _, oldEnvPara := range w.agentInitOps.Envs {
		var exist bool
		for _, newEnvPara := range newOpt.Envs {
			if newEnvPara.Namespace == oldEnvPara.Namespace {
				exist = true
			}
		}
		if !exist {
			glog.Infof("stop env %s ...", oldEnvPara.Namespace)
			close(w.repoStopChans[oldEnvPara.Namespace])
		}
	}
}

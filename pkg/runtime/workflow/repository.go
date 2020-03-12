package workflow

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"time"

	wf "github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	"github.com/argoproj/argo/pkg/client/clientset/versioned"
	informers "github.com/argoproj/argo/pkg/client/informers/externalversions/workflow/v1alpha1"
	"github.com/argoproj/argo/workflow/util"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	log "github.com/sirupsen/logrus"
)

// Log  is the struct of log.
type Log struct {
	DisplayName string
	Pod         string
	Message     string
	Time        time.Time
}

// Repo is the workflow repository it always syncronizes the workflows as the storage at background.
type Repo struct {
	c   *controller
	s   *storage
	ac  versioned.Interface
	kc  kubernetes.Interface
	log *log.Entry
}

// NewRepo create a new workflow repository.
func NewRepo(
	argoClientset versioned.Interface, argoInformer informers.WorkflowInformer, kubeClientset kubernetes.Interface) *Repo {
	s := newStorage(argoInformer)
	c := newController(argoClientset, argoInformer, s)

	repo := &Repo{
		c:  c,
		s:  s,
		ac: argoClientset,
		kc: kubeClientset,
	}
	return repo
}

// WaitForSync wait for syncronize.
func (a *Repo) WaitForSync(stop chan struct{}) error {
	// run the controller
	go a.c.run(stop)

	return a.c.waitForSynced(stop)
}

// Get get the workflow by the key, the format is "namespace/key", and if doesn't exist it return nil.
func (a *Repo) Get(key string) (runtime.Object, error) {
	w, err := a.s.GetWorkflow(key)
	if err != nil {
		a.log.Errorf("failed to get the '%s' workflow.", key)
		return nil, err
	}

	return w, nil
}

// Search get workflows which are matched with pattern.
func (a *Repo) Search(namespace, pattern string) []runtime.Object {
	var (
		wfs = make([]runtime.Object, 0)
	)

	keys := a.s.List(fmt.Sprintf("%s/*%s*", namespace, pattern))
	for _, k := range keys {
		w, err := a.s.GetWorkflow(k)
		if err != nil {
			a.log.Errorf("failed to get the '%s' workflow.", k)
			return nil
		}

		wfs = append(wfs, w)
	}
	return wfs
}

// Delete delete the workflow by the key.
func (a *Repo) Delete(key string) error {
	if key == "" {
		return fmt.Errorf("there is no key to delete")
	}

	ns, n, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	a.log.Debugf("delete '%s' workflow", key)
	err = a.ac.ArgoprojV1alpha1().Workflows(ns).Delete(n, &metav1.DeleteOptions{})
	if err != nil {
		return err
	}

	return nil
}

// Logs get the channel to recieve Logs from a Argo workflow.
func (a *Repo) Logs(ctx context.Context, key string) (<-chan Log, error) {
	w, err := a.s.GetWorkflow(key)
	if err != nil {
		return nil, err
	}

	var (
		ch = make(chan Log, 100)
	)
	// get logs and send to the channel.
	err = a.logsWorkflow(ctx, ch, w)
	if err != nil {
		return nil, err
	}

	return ch, nil
}

func (a *Repo) logsWorkflow(ctx context.Context, ch chan<- Log, w *wf.Workflow) error {
	err := util.DecompressWorkflow(w)
	if err != nil {
		a.log.Error(err)
		return err
	}

	// node is the unit of the executed step.
	var nodes []wf.NodeStatus
	for _, n := range w.Status.Nodes {
		if n.Type == wf.NodeTypePod && n.Phase != wf.NodeError {
			nodes = append(nodes, n)
		}
	}

	for _, n := range nodes {
		ns, n, dn := w.Namespace, n.ID, n.DisplayName

		// get logs from nodes at background.
		go func() {
			a.log.Tracef("log '%s' node.", n)
			err := a.logsPod(ctx, ch, ns, n, dn)
			if err != nil {
				a.log.Errorf("couldn't get logs from '%s' node: %s.", n, err)
				return
			}
		}()
	}

	return nil
}

func (a *Repo) logsPod(ctx context.Context, ch chan<- Log, ns string, n string, dn string) error {
	const (
		mainContainerName = "main"
	)
	var (
		key = ns + "/" + n
	)

	s, err := a.kc.CoreV1().Pods(ns).GetLogs(n, &corev1.PodLogOptions{
		Container:  mainContainerName,
		Follow:     true,
		Timestamps: true, // add an RFC3339 or RFC3339Nano timestamp at the beginning
	}).Stream()
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(s)
	for {
		select {
		case <-ctx.Done():
			a.log.WithField("key", key).Trace("the context is closed.")
			return nil

		default:
			if !scanner.Scan() {
				a.log.WithField("key", key).Trace("finished to logs.")
				return nil
			}

			line := scanner.Text()
			t, m := splitTimeAndMessage(line)

			time, err := time.Parse(time.RFC3339, t)
			if err != nil {
				a.log.WithField("key", key).Warnf("can't parse the timestamp: %s", err)
				continue
			}

			ch <- Log{
				DisplayName: dn,
				Pod:         n,
				Message:     m,
				Time:        time,
			}
		}
	}
}

// splitTimeAndMessage split the log from Kubernetes into time and message.
func splitTimeAndMessage(l string) (string, string) {
	parts := strings.SplitN(l, " ", 2)
	return parts[0], parts[1]
}

func getNodeDisplayName(n wf.NodeStatus) string {
	dn := n.DisplayName
	if dn == "" {
		dn = n.Name
	}
	return dn
}
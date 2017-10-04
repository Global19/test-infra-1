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

package plank

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"text/template"
	"time"

	"github.com/bwmarrin/snowflake"

	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/pjutil"
)

type fca struct {
	sync.Mutex
	c *config.Config
}

func newFakeConfigAgent(t *testing.T, maxConcurrency int) *fca {
	presubmits := []config.Presubmit{
		{
			Name: "test-bazel-build",
			RunAfterSuccess: []config.Presubmit{
				{
					Name:         "test-kubeadm-cloud",
					RunIfChanged: "^(cmd/kubeadm|build/debs).*$",
				},
			},
		},
		{
			Name: "test-e2e",
			RunAfterSuccess: []config.Presubmit{
				{
					Name: "push-image",
				},
			},
		},
		{
			Name: "test-bazel-test",
		},
	}
	if err := config.SetRegexes(presubmits); err != nil {
		t.Fatal(err)
	}
	presubmitMap := map[string][]config.Presubmit{
		"kubernetes/kubernetes": presubmits,
	}

	return &fca{
		c: &config.Config{
			Plank: config.Plank{
				JobURLTemplate: template.Must(template.New("test").Parse("{{.Metadata.Name}}/{{.Status.State}}")),
				MaxConcurrency: maxConcurrency,
			},
			Presubmits: presubmitMap,
		},
	}
}

func (f *fca) Config() *config.Config {
	f.Lock()
	defer f.Unlock()
	return f.c
}

type fkc struct {
	sync.Mutex
	prowjobs []kube.ProwJob
	pods     []kube.Pod
	err      error
}

func (f *fkc) CreateProwJob(pj kube.ProwJob) (kube.ProwJob, error) {
	f.Lock()
	defer f.Unlock()
	f.prowjobs = append(f.prowjobs, pj)
	return pj, nil
}

func (f *fkc) ListProwJobs(map[string]string) ([]kube.ProwJob, error) {
	f.Lock()
	defer f.Unlock()
	return f.prowjobs, nil
}

func (f *fkc) ReplaceProwJob(name string, job kube.ProwJob) (kube.ProwJob, error) {
	f.Lock()
	defer f.Unlock()
	for i := range f.prowjobs {
		if f.prowjobs[i].Metadata.Name == name {
			f.prowjobs[i] = job
			return job, nil
		}
	}
	return kube.ProwJob{}, fmt.Errorf("did not find prowjob %s", name)
}

func (f *fkc) CreatePod(pod kube.Pod) (kube.Pod, error) {
	f.Lock()
	defer f.Unlock()
	if f.err != nil {
		return kube.Pod{}, f.err
	}
	f.pods = append(f.pods, pod)
	return pod, nil
}

func (f *fkc) ListPods(map[string]string) ([]kube.Pod, error) {
	f.Lock()
	defer f.Unlock()
	return f.pods, nil
}

func (f *fkc) DeletePod(name string) error {
	f.Lock()
	defer f.Unlock()
	for i := range f.pods {
		if f.pods[i].Metadata.Name == name {
			f.pods = append(f.pods[:i], f.pods[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("did not find pod %s", name)
}

type fghc struct {
	sync.Mutex
	changes []github.PullRequestChange
	err     error
}

func (f *fghc) GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error) {
	f.Lock()
	defer f.Unlock()
	return f.changes, f.err
}

func (f *fghc) BotName() (string, error)                                  { return "bot", nil }
func (f *fghc) CreateStatus(org, repo, ref string, s github.Status) error { return nil }
func (f *fghc) ListIssueComments(org, repo string, number int) ([]github.IssueComment, error) {
	return nil, nil
}
func (f *fghc) CreateComment(org, repo string, number int, comment string) error { return nil }
func (f *fghc) DeleteComment(org, repo string, ID int) error                     { return nil }
func (f *fghc) EditComment(org, repo string, ID int, comment string) error       { return nil }

func TestTerminateDupes(t *testing.T) {
	now := time.Now()
	var testcases = []struct {
		name      string
		job       string
		startTime time.Time
		complete  bool

		shouldTerminate bool
	}{
		{
			name:            "newest",
			job:             "j1",
			startTime:       now.Add(-time.Minute),
			complete:        false,
			shouldTerminate: false,
		},
		{
			name:            "old",
			job:             "j1",
			startTime:       now.Add(-time.Hour),
			complete:        false,
			shouldTerminate: true,
		},
		{
			name:            "older",
			job:             "j1",
			startTime:       now.Add(-2 * time.Hour),
			complete:        false,
			shouldTerminate: true,
		},
		{
			name:            "complete",
			job:             "j1",
			startTime:       now.Add(-3 * time.Hour),
			complete:        true,
			shouldTerminate: false,
		},
		{
			name:            "newest j2",
			job:             "j2",
			startTime:       now.Add(-time.Minute),
			complete:        false,
			shouldTerminate: false,
		},
		{
			name:            "old j2",
			job:             "j2",
			startTime:       now.Add(-time.Hour),
			complete:        false,
			shouldTerminate: true,
		},
		{
			name:            "old j3",
			job:             "j3",
			startTime:       now.Add(-time.Hour),
			complete:        false,
			shouldTerminate: true,
		},
		{
			name:            "newest j3",
			job:             "j3",
			startTime:       now.Add(-time.Minute),
			complete:        false,
			shouldTerminate: false,
		},
	}
	fkc := &fkc{}
	c := Controller{kc: fkc, pkc: fkc}
	for _, tc := range testcases {
		var pj = kube.ProwJob{
			Metadata: kube.ObjectMeta{Name: tc.name},
			Spec: kube.ProwJobSpec{
				Type: kube.PresubmitJob,
				Job:  tc.job,
				Refs: kube.Refs{Pulls: []kube.Pull{{}}},
			},
			Status: kube.ProwJobStatus{
				StartTime: tc.startTime,
			},
		}
		if tc.complete {
			pj.Status.CompletionTime = now
		}
		fkc.prowjobs = append(fkc.prowjobs, pj)
	}
	if err := c.terminateDupes(fkc.prowjobs); err != nil {
		t.Fatalf("Error terminating dupes: %v", err)
	}
	for i := range testcases {
		terminated := fkc.prowjobs[i].Status.State == kube.AbortedState
		if terminated != testcases[i].shouldTerminate {
			t.Errorf("Wrong termination for %s", testcases[i].name)
		}
	}
}

func handleTot(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "42")
}

func TestSyncNonPendingJobs(t *testing.T) {
	var testcases = []struct {
		name string

		pj             kube.ProwJob
		pendingJobs    map[string]int
		maxConcurrency int
		pods           []kube.Pod
		podErr         error

		expectedState      kube.ProwJobState
		expectedPodHasName bool
		expectedNumPods    int
		expectedComplete   bool
		expectedCreatedPJs int
		expectedReport     bool
		expectedURL        string
		expectedBuildID    string
		expectError        bool
	}{
		{
			name: "completed prow job",
			pj: kube.ProwJob{
				Status: kube.ProwJobStatus{
					CompletionTime: time.Now(),
					State:          kube.FailureState,
				},
			},
			expectedState:    kube.FailureState,
			expectedComplete: true,
		},
		{
			name: "completed prow job, missing pod",
			pj: kube.ProwJob{
				Status: kube.ProwJobStatus{
					CompletionTime: time.Now(),
					State:          kube.FailureState,
					PodName:        "boop-41",
				},
			},
			expectedState:    kube.FailureState,
			expectedNumPods:  0,
			expectedComplete: true,
		},
		{
			name: "start new pod",
			pj: kube.ProwJob{
				Metadata: kube.ObjectMeta{
					Name: "blabla",
				},
				Spec: kube.ProwJobSpec{
					Job:  "boop",
					Type: kube.PeriodicJob,
				},
				Status: kube.ProwJobStatus{
					State: kube.TriggeredState,
				},
			},
			expectedState:      kube.PendingState,
			expectedPodHasName: true,
			expectedNumPods:    1,
			expectedReport:     true,
			expectedURL:        "blabla/pending",
		},
		{
			name: "pod with a max concurrency of 1",
			pj: kube.ProwJob{
				Spec: kube.ProwJobSpec{
					Job:            "same",
					MaxConcurrency: 1,
				},
				Status: kube.ProwJobStatus{
					State: kube.TriggeredState,
				},
			},
			pendingJobs: map[string]int{
				"same": 1,
			},
			pods: []kube.Pod{
				{
					Metadata: kube.ObjectMeta{
						Name: "same-42",
					},
					Status: kube.PodStatus{
						Phase: kube.PodRunning,
					},
				},
			},
			expectedState:   kube.TriggeredState,
			expectedNumPods: 1,
		},
		{
			name: "do not exceed global maxconcurrency",
			pj: kube.ProwJob{
				Metadata: kube.ObjectMeta{
					Name: "beer",
				},
				Spec: kube.ProwJobSpec{
					Job:  "same",
					Type: kube.PeriodicJob,
				},
				Status: kube.ProwJobStatus{
					State: kube.TriggeredState,
				},
			},
			maxConcurrency: 20,
			pendingJobs:    map[string]int{"motherearth": 10, "allagash": 8, "krusovice": 2},
			expectedState:  kube.TriggeredState,
		},
		{
			name: "global maxconcurrency allows new jobs when possible",
			pj: kube.ProwJob{
				Metadata: kube.ObjectMeta{
					Name: "beer",
				},
				Spec: kube.ProwJobSpec{
					Job:  "same",
					Type: kube.PeriodicJob,
				},
				Status: kube.ProwJobStatus{
					State: kube.TriggeredState,
				},
			},
			maxConcurrency:  21,
			pendingJobs:     map[string]int{"motherearth": 10, "allagash": 8, "krusovice": 2},
			expectedState:   kube.PendingState,
			expectedNumPods: 1,
			expectedReport:  true,
			expectedURL:     "beer/pending",
		},
		{
			name: "unprocessable prow job",
			pj: kube.ProwJob{
				Spec: kube.ProwJobSpec{
					Job:  "boop",
					Type: kube.PeriodicJob,
				},
				Status: kube.ProwJobStatus{
					State: kube.TriggeredState,
				},
			},
			podErr:           kube.NewUnprocessableEntityError(errors.New("no way jose")),
			expectedState:    kube.ErrorState,
			expectedComplete: true,
			expectedReport:   true,
		},
		{
			name: "conflict error starting pod",
			pj: kube.ProwJob{
				Spec: kube.ProwJobSpec{
					Job:  "boop",
					Type: kube.PeriodicJob,
				},
				Status: kube.ProwJobStatus{
					State: kube.TriggeredState,
				},
			},
			podErr:        kube.NewConflictError(errors.New("no way jose")),
			expectedState: kube.TriggeredState,
			expectError:   true,
		},
		{
			name: "unknown error starting pod",
			pj: kube.ProwJob{
				Spec: kube.ProwJobSpec{
					Job:  "boop",
					Type: kube.PeriodicJob,
				},
				Status: kube.ProwJobStatus{
					State: kube.TriggeredState,
				},
			},
			podErr:        errors.New("no way unknown jose"),
			expectedState: kube.TriggeredState,
			expectError:   true,
		},
		{
			name: "running pod, failed prowjob update",
			pj: kube.ProwJob{
				Metadata: kube.ObjectMeta{
					Name: "foo",
				},
				Spec: kube.ProwJobSpec{
					Job:  "boop",
					Type: kube.PeriodicJob,
				},
				Status: kube.ProwJobStatus{
					State: kube.TriggeredState,
				},
			},
			pods: []kube.Pod{
				{
					Metadata: kube.ObjectMeta{
						Name: "foo",
					},
					Spec: kube.PodSpec{
						Containers: []kube.Container{
							{
								Env: []kube.EnvVar{
									{
										Name:  "BUILD_NUMBER",
										Value: "0987654321",
									},
								},
							},
						},
					},
					Status: kube.PodStatus{
						Phase: kube.PodRunning,
					},
				},
			},
			expectedState:   kube.PendingState,
			expectedNumPods: 1,
			expectedReport:  true,
			expectedURL:     "foo/pending",
			expectedBuildID: "0987654321",
		},
	}
	for _, tc := range testcases {
		totServ := httptest.NewServer(http.HandlerFunc(handleTot))
		defer totServ.Close()
		pm := make(map[string]kube.Pod)
		for i := range tc.pods {
			pm[tc.pods[i].Metadata.Name] = tc.pods[i]
		}
		fc := &fkc{
			prowjobs: []kube.ProwJob{tc.pj},
		}
		fpc := &fkc{
			pods: tc.pods,
			err:  tc.podErr,
		}
		c := Controller{
			kc:          fc,
			pkc:         fpc,
			ca:          newFakeConfigAgent(t, tc.maxConcurrency),
			totURL:      totServ.URL,
			pendingJobs: make(map[string]int),
		}
		if tc.pendingJobs != nil {
			c.pendingJobs = tc.pendingJobs
		}

		reports := make(chan kube.ProwJob, 100)
		if err := c.syncNonPendingJob(tc.pj, pm, reports); (err != nil) != tc.expectError {
			if tc.expectError {
				t.Errorf("for case %q expected an error, but got none", tc.name)
			} else {
				t.Errorf("for case %q got an unexpected error: %v", tc.name, err)
			}
			continue
		}
		close(reports)

		actual := fc.prowjobs[0]
		if actual.Status.State != tc.expectedState {
			t.Errorf("for case %q got state %v", tc.name, actual.Status.State)
		}
		if (actual.Status.PodName == "") && tc.expectedPodHasName {
			t.Errorf("for case %q got no pod name, expected one", tc.name)
		}
		if len(fpc.pods) != tc.expectedNumPods {
			t.Errorf("for case %q got %d pods", tc.name, len(fpc.pods))
		}
		if actual.Complete() != tc.expectedComplete {
			t.Errorf("for case %q got wrong completion", tc.name)
		}
		if len(fc.prowjobs) != tc.expectedCreatedPJs+1 {
			t.Errorf("for case %q got %d created prowjobs", tc.name, len(fc.prowjobs)-1)
		}
		if tc.expectedReport && len(reports) != 1 {
			t.Errorf("for case %q wanted one report but got %d", tc.name, len(reports))
		}
		if !tc.expectedReport && len(reports) != 0 {
			t.Errorf("for case %q did not wany any reports but got %d", tc.name, len(reports))
		}
		if tc.expectedReport {
			r := <-reports

			if got, want := r.Status.URL, tc.expectedURL; got != want {
				t.Errorf("for case %q, report.Status.URL: got %q, want %q", tc.name, got, want)
			}
			if got, want := r.Status.BuildID, tc.expectedBuildID; want != "" && got != want {
				t.Errorf("for case %q, report.Status.BuildID: got %q, want %q", tc.name, got, want)
			}
		}
	}
}

func TestSyncPendingJob(t *testing.T) {
	var testcases = []struct {
		name string

		pj   kube.ProwJob
		pods []kube.Pod
		err  error

		expectedState      kube.ProwJobState
		expectedNumPods    int
		expectedComplete   bool
		expectedCreatedPJs int
		expectedReport     bool
		expectedURL        string
	}{
		{
			name: "reset when pod goes missing",
			pj: kube.ProwJob{
				Metadata: kube.ObjectMeta{
					Name: "boop-41",
				},
				Spec: kube.ProwJobSpec{
					Type: kube.PostsubmitJob,
				},
				Status: kube.ProwJobStatus{
					State:   kube.PendingState,
					PodName: "boop-41",
				},
			},
			expectedState:   kube.PendingState,
			expectedReport:  true,
			expectedNumPods: 1,
			expectedURL:     "boop-41/pending",
		},
		{
			name: "delete pod in unknown state",
			pj: kube.ProwJob{
				Metadata: kube.ObjectMeta{
					Name: "boop-41",
				},
				Status: kube.ProwJobStatus{
					State:   kube.PendingState,
					PodName: "boop-41",
				},
			},
			pods: []kube.Pod{
				{
					Metadata: kube.ObjectMeta{
						Name: "boop-41",
					},
					Status: kube.PodStatus{
						Phase: kube.PodUnknown,
					},
				},
			},
			expectedState:   kube.PendingState,
			expectedNumPods: 0,
		},
		{
			name: "succeeded pod",
			pj: kube.ProwJob{
				Metadata: kube.ObjectMeta{
					Name: "boop-42",
				},
				Spec: kube.ProwJobSpec{
					Type:            kube.BatchJob,
					RunAfterSuccess: []kube.ProwJobSpec{{}},
				},
				Status: kube.ProwJobStatus{
					State:   kube.PendingState,
					PodName: "boop-42",
				},
			},
			pods: []kube.Pod{
				{
					Metadata: kube.ObjectMeta{
						Name: "boop-42",
					},
					Status: kube.PodStatus{
						Phase: kube.PodSucceeded,
					},
				},
			},
			expectedComplete:   true,
			expectedState:      kube.SuccessState,
			expectedNumPods:    1,
			expectedCreatedPJs: 1,
			expectedReport:     true,
			expectedURL:        "boop-42/success",
		},
		{
			name: "failed pod",
			pj: kube.ProwJob{
				Metadata: kube.ObjectMeta{
					Name: "boop-42",
				},
				Spec: kube.ProwJobSpec{
					Type: kube.PresubmitJob,
					Refs: kube.Refs{
						Org: "kubernetes", Repo: "kubernetes",
						BaseRef: "baseref", BaseSHA: "basesha",
						Pulls: []kube.Pull{{Number: 100, Author: "me", SHA: "sha"}},
					},
					RunAfterSuccess: []kube.ProwJobSpec{{}},
				},
				Status: kube.ProwJobStatus{
					State:   kube.PendingState,
					PodName: "boop-42",
				},
			},
			pods: []kube.Pod{
				{
					Metadata: kube.ObjectMeta{
						Name: "boop-42",
					},
					Status: kube.PodStatus{
						Phase: kube.PodFailed,
					},
				},
			},
			expectedComplete: true,
			expectedState:    kube.FailureState,
			expectedNumPods:  1,
			expectedReport:   true,
			expectedURL:      "boop-42/failure",
		},
		{
			name: "delete evicted pod",
			pj: kube.ProwJob{
				Metadata: kube.ObjectMeta{
					Name: "boop-42",
				},
				Status: kube.ProwJobStatus{
					State:   kube.PendingState,
					PodName: "boop-42",
				},
			},
			pods: []kube.Pod{
				{
					Metadata: kube.ObjectMeta{
						Name: "boop-42",
					},
					Status: kube.PodStatus{
						Phase:  kube.PodFailed,
						Reason: kube.Evicted,
					},
				},
			},
			expectedComplete: false,
			expectedState:    kube.PendingState,
			expectedNumPods:  0,
		},
		{
			name: "running pod",
			pj: kube.ProwJob{
				Metadata: kube.ObjectMeta{
					Name: "boop-42",
				},
				Spec: kube.ProwJobSpec{
					RunAfterSuccess: []kube.ProwJobSpec{{}},
				},
				Status: kube.ProwJobStatus{
					State:   kube.PendingState,
					PodName: "boop-42",
				},
			},
			pods: []kube.Pod{
				{
					Metadata: kube.ObjectMeta{
						Name: "boop-42",
					},
					Status: kube.PodStatus{
						Phase: kube.PodRunning,
					},
				},
			},
			expectedState:   kube.PendingState,
			expectedNumPods: 1,
		},
		{
			name: "pod changes url status",
			pj: kube.ProwJob{
				Metadata: kube.ObjectMeta{
					Name: "boop-42",
				},
				Spec: kube.ProwJobSpec{
					RunAfterSuccess: []kube.ProwJobSpec{{}},
				},
				Status: kube.ProwJobStatus{
					State:   kube.PendingState,
					PodName: "boop-42",
					URL:     "boop-42/pending",
				},
			},
			pods: []kube.Pod{
				{
					Metadata: kube.ObjectMeta{
						Name: "boop-42",
					},
					Status: kube.PodStatus{
						Phase: kube.PodSucceeded,
					},
				},
			},
			expectedComplete:   true,
			expectedState:      kube.SuccessState,
			expectedNumPods:    1,
			expectedCreatedPJs: 1,
			expectedReport:     true,
			expectedURL:        "boop-42/success",
		},
		{
			name: "unprocessable prow job",
			pj: kube.ProwJob{
				Metadata: kube.ObjectMeta{
					Name: "jose",
				},
				Spec: kube.ProwJobSpec{
					Job:  "boop",
					Type: kube.PostsubmitJob,
				},
				Status: kube.ProwJobStatus{
					State: kube.PendingState,
				},
			},
			err:              kube.NewUnprocessableEntityError(errors.New("no way jose")),
			expectedState:    kube.ErrorState,
			expectedComplete: true,
			expectedReport:   true,
			expectedURL:      "jose/error",
		},
	}
	for _, tc := range testcases {
		totServ := httptest.NewServer(http.HandlerFunc(handleTot))
		defer totServ.Close()
		pm := make(map[string]kube.Pod)
		for i := range tc.pods {
			pm[tc.pods[i].Metadata.Name] = tc.pods[i]
		}
		fc := &fkc{
			prowjobs: []kube.ProwJob{tc.pj},
		}
		fpc := &fkc{
			pods: tc.pods,
			err:  tc.err,
		}
		c := Controller{
			kc:          fc,
			pkc:         fpc,
			ca:          newFakeConfigAgent(t, 0),
			totURL:      totServ.URL,
			pendingJobs: make(map[string]int),
		}

		reports := make(chan kube.ProwJob, 100)
		if err := c.syncPendingJob(tc.pj, pm, reports); err != nil {
			t.Errorf("for case %q got an error: %v", tc.name, err)
			continue
		}
		close(reports)

		actual := fc.prowjobs[0]
		if actual.Status.State != tc.expectedState {
			t.Errorf("for case %q got state %v", tc.name, actual.Status.State)
		}
		if len(fpc.pods) != tc.expectedNumPods {
			t.Errorf("for case %q got %d pods, expected %d", tc.name, len(fpc.pods), tc.expectedNumPods)
		}
		if actual.Complete() != tc.expectedComplete {
			t.Errorf("for case %q got wrong completion", tc.name)
		}
		if len(fc.prowjobs) != tc.expectedCreatedPJs+1 {
			t.Errorf("for case %q got %d created prowjobs", tc.name, len(fc.prowjobs)-1)
		}
		if tc.expectedReport && len(reports) != 1 {
			t.Errorf("for case %q wanted one report but got %d", tc.name, len(reports))
		}
		if !tc.expectedReport && len(reports) != 0 {
			t.Errorf("for case %q did not wany any reports but got %d", tc.name, len(reports))
		}
		if tc.expectedReport {
			r := <-reports

			if got, want := r.Status.URL, tc.expectedURL; got != want {
				t.Errorf("for case %q, report.Status.URL: got %q, want %q", tc.name, got, want)
			}
		}
	}
}

// TestPeriodic walks through the happy path of a periodic job.
func TestPeriodic(t *testing.T) {
	per := config.Periodic{
		Name:  "ci-periodic-job",
		Agent: "kubernetes",
		Spec: &kube.PodSpec{
			Containers: []kube.Container{{}},
		},
		RunAfterSuccess: []config.Periodic{
			{
				Name:  "ci-periodic-job-2",
				Agent: "kubernetes",
				Spec:  &kube.PodSpec{},
			},
		},
	}

	totServ := httptest.NewServer(http.HandlerFunc(handleTot))
	defer totServ.Close()
	fc := &fkc{
		prowjobs: []kube.ProwJob{pjutil.NewProwJob(pjutil.PeriodicSpec(per))},
	}
	c := Controller{
		kc:          fc,
		pkc:         fc,
		ca:          newFakeConfigAgent(t, 0),
		totURL:      totServ.URL,
		pendingJobs: make(map[string]int),
		lock:        sync.RWMutex{},
	}

	if err := c.Sync(); err != nil {
		t.Fatalf("Error on first sync: %v", err)
	}
	if fc.prowjobs[0].Spec.PodSpec.Containers[0].Name != "" {
		t.Fatal("Sync step updated the TPR spec.")
	}
	if len(fc.pods) != 1 {
		t.Fatal("Didn't create pod on first sync.")
	}
	if len(fc.pods[0].Spec.Containers) != 1 {
		t.Fatal("Wiped container list.")
	}
	if len(fc.pods[0].Spec.Containers[0].Env) == 0 {
		t.Fatal("Container has no env set.")
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Error on second sync: %v", err)
	}
	if len(fc.pods) != 1 {
		t.Fatalf("Wrong number of pods after second sync: %d", len(fc.pods))
	}
	fc.pods[0].Status.Phase = kube.PodSucceeded
	if err := c.Sync(); err != nil {
		t.Fatalf("Error on third sync: %v", err)
	}
	if !fc.prowjobs[0].Complete() {
		t.Fatal("Prow job didn't complete.")
	}
	if fc.prowjobs[0].Status.State != kube.SuccessState {
		t.Fatalf("Should be success: %v", fc.prowjobs[0].Status.State)
	}
	if len(fc.prowjobs) != 2 {
		t.Fatalf("Wrong number of prow jobs: %d", len(fc.prowjobs))
	}
	if err := c.Sync(); err != nil {
		t.Fatalf("Error on fourth sync: %v", err)
	}
}

func TestRunAfterSuccessCanRun(t *testing.T) {
	tests := []struct {
		name string

		parent *kube.ProwJob
		child  *kube.ProwJob

		changes []github.PullRequestChange
		err     error

		expected bool
	}{
		{
			name: "child does not require specific changes",
			parent: &kube.ProwJob{
				Spec: kube.ProwJobSpec{
					Job:  "test-e2e",
					Type: kube.PresubmitJob,
					Refs: kube.Refs{
						Org:  "kubernetes",
						Repo: "kubernetes",
						Pulls: []kube.Pull{
							{Number: 123},
						},
					},
				},
			},
			child: &kube.ProwJob{
				Spec: kube.ProwJobSpec{
					Job: "push-image",
				},
			},
			expected: true,
		},
		{
			name: "child requires specific changes that are done",
			parent: &kube.ProwJob{
				Spec: kube.ProwJobSpec{
					Job:  "test-bazel-build",
					Type: kube.PresubmitJob,
					Refs: kube.Refs{
						Org:  "kubernetes",
						Repo: "kubernetes",
						Pulls: []kube.Pull{
							{Number: 123},
						},
					},
				},
			},
			child: &kube.ProwJob{
				Spec: kube.ProwJobSpec{
					Job: "test-kubeadm-cloud",
				},
			},
			changes: []github.PullRequestChange{
				{Filename: "cmd/kubeadm/kubeadm.go"},
				{Filename: "vendor/BUILD"},
				{Filename: ".gitatrributes"},
			},
			expected: true,
		},
		{
			name: "child requires specific changes that are not done",
			parent: &kube.ProwJob{
				Spec: kube.ProwJobSpec{
					Job:  "test-bazel-build",
					Type: kube.PresubmitJob,
					Refs: kube.Refs{
						Org:  "kubernetes",
						Repo: "kubernetes",
						Pulls: []kube.Pull{
							{Number: 123},
						},
					},
				},
			},
			child: &kube.ProwJob{
				Spec: kube.ProwJobSpec{
					Job: "test-kubeadm-cloud",
				},
			},
			changes: []github.PullRequestChange{
				{Filename: "vendor/BUILD"},
				{Filename: ".gitatrributes"},
			},
			expected: false,
		},
	}

	for _, test := range tests {
		t.Logf("scenario %q", test.name)

		fakeGH := &fghc{
			changes: test.changes,
			err:     test.err,
		}

		got := RunAfterSuccessCanRun(test.parent, test.child, newFakeConfigAgent(t, 0), fakeGH)
		if got != test.expected {
			t.Errorf("expected to run: %t, got: %t", test.expected, got)
		}
	}
}

func TestMaxConcurrencyWithNewlyTriggeredJobs(t *testing.T) {
	tests := []struct {
		name         string
		pjs          []kube.ProwJob
		pendingJobs  map[string]int
		expectedPods int
	}{
		{
			name: "avoid starting a triggered job",
			pjs: []kube.ProwJob{
				{
					Spec: kube.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           kube.PostsubmitJob,
						MaxConcurrency: 1,
					},
					Status: kube.ProwJobStatus{
						State: kube.TriggeredState,
					},
				},
				{
					Spec: kube.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           kube.PostsubmitJob,
						MaxConcurrency: 1,
					},
					Status: kube.ProwJobStatus{
						State: kube.TriggeredState,
					},
				},
			},
			pendingJobs:  make(map[string]int),
			expectedPods: 1,
		},
		{
			name: "both triggered jobs can start",
			pjs: []kube.ProwJob{
				{
					Spec: kube.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           kube.PostsubmitJob,
						MaxConcurrency: 2,
					},
					Status: kube.ProwJobStatus{
						State: kube.TriggeredState,
					},
				},
				{
					Spec: kube.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           kube.PostsubmitJob,
						MaxConcurrency: 2,
					},
					Status: kube.ProwJobStatus{
						State: kube.TriggeredState,
					},
				},
			},
			pendingJobs:  make(map[string]int),
			expectedPods: 2,
		},
		{
			name: "no triggered job can start",
			pjs: []kube.ProwJob{
				{
					Spec: kube.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           kube.PostsubmitJob,
						MaxConcurrency: 5,
					},
					Status: kube.ProwJobStatus{
						State: kube.TriggeredState,
					},
				},
				{
					Spec: kube.ProwJobSpec{
						Job:            "test-bazel-build",
						Type:           kube.PostsubmitJob,
						MaxConcurrency: 5,
					},
					Status: kube.ProwJobStatus{
						State: kube.TriggeredState,
					},
				},
			},
			pendingJobs:  map[string]int{"test-bazel-build": 5},
			expectedPods: 0,
		},
	}

	for _, test := range tests {
		jobs := make(chan kube.ProwJob, len(test.pjs))
		for _, pj := range test.pjs {
			jobs <- pj
		}
		close(jobs)

		fc := &fkc{
			prowjobs: test.pjs,
		}
		fpc := &fkc{}
		n, err := snowflake.NewNode(1)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		c := Controller{
			kc:          fc,
			pkc:         fpc,
			ca:          newFakeConfigAgent(t, 0),
			node:        n,
			pendingJobs: test.pendingJobs,
		}

		reports := make(chan kube.ProwJob, len(test.pjs))
		errors := make(chan error, len(test.pjs))
		pm := make(map[string]kube.Pod)

		syncProwJobs(c.syncNonPendingJob, jobs, reports, errors, pm)
		if len(fpc.pods) != test.expectedPods {
			t.Errorf("expected pods: %d, got: %d", test.expectedPods, len(fpc.pods))
		}
	}
}
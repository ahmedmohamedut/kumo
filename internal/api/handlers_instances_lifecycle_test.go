package api_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
)

// consoleBody is the shape of GET /instances/{id}/console-output.
type consoleBody struct {
	InstanceID string `json:"instance_id"`
	Output     string `json:"output"`
}

// launchedInstance launches through the API and waits for the boot, so a lifecycle
// test starts from an instance that is really running.
func launchedInstance(t *testing.T, ir instanceRouter, imageID, name string) instanceBody {
	t.Helper()

	accepted := doWithToken(t, ir.router, http.MethodPost, "/api/v1/instances", fmt.Sprintf(
		`{"name":%q,"image_id":%q,"instance_type":"kumo.small","root_disk_gb":8}`, name, imageID),
		ir.tokens.access)
	if accepted.Code != http.StatusAccepted {
		t.Fatalf("POST /instances %q status = %d, want 202 (body %s)", name, accepted.Code, accepted.Body)
	}

	var launched instanceBody
	decode(t, accepted, &launched)
	if launched.TaskID == nil {
		t.Fatalf("launching %q returned no task_id", name)
	}
	waitForTaskState(t, ir.router, ir.tokens.access, *launched.TaskID, "succeeded")
	return launched
}

// action runs a lifecycle endpoint and answers the instance it queued, checking the
// parts every one of them promises: 202, a task, and a Location that points at it.
func action(t *testing.T, ir instanceRouter, method, path string) instanceBody {
	t.Helper()

	recorder := doWithToken(t, ir.router, method, path, "", ir.tokens.access)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("%s %s status = %d, want 202 (body %s)", method, path, recorder.Code, recorder.Body)
	}

	var queued instanceBody
	decode(t, recorder, &queued)
	if queued.TaskID == nil {
		t.Fatalf("%s %s queued no task", method, path)
	}
	if got, want := recorder.Header().Get("Location"), "/api/v1/tasks/"+*queued.TaskID; got != want {
		t.Errorf("%s %s Location = %q, want %q", method, path, got, want)
	}
	return queued
}

// stateOf reads the state the API reports for an instance.
func stateOf(t *testing.T, ir instanceRouter, id string) string {
	t.Helper()

	recorder := doWithToken(t, ir.router, http.MethodGet, "/api/v1/instances/"+id, "", ir.tokens.access)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /instances/%s status = %d, want 200 (body %s)", id, recorder.Code, recorder.Body)
	}
	var found instanceBody
	decode(t, recorder, &found)
	return found.State
}

func TestLifecycleActionsWalkAnInstanceThroughItsStates(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)
	launched := launchedInstance(t, ir, imageID, "web-01")
	base := "/api/v1/instances/" + launched.ID

	// A stop is a request and a wait, so it answers 202 with the transitional state
	// and a task rather than blocking on a guest.
	stopped := action(t, ir, http.MethodPost, base+"/stop")
	if stopped.State != string(instance.StateStopping) {
		t.Errorf("the stop answered %q, want stopping", stopped.State)
	}
	waitForTaskState(t, ir.router, ir.tokens.access, *stopped.TaskID, "succeeded")
	if got := stateOf(t, ir, launched.ID); got != string(instance.StateStopped) {
		t.Fatalf("state = %q after the stop, want stopped", got)
	}

	// A start keeps the row saying stopped: pending there means a launch, and the
	// guest takes as long to come up as it takes.
	started := action(t, ir, http.MethodPost, base+"/start")
	if started.State != string(instance.StateStopped) {
		t.Errorf("the start answered %q, want the state it had", started.State)
	}
	waitForTaskState(t, ir.router, ir.tokens.access, *started.TaskID, "succeeded")
	if got := stateOf(t, ir, launched.ID); got != string(instance.StateRunning) {
		t.Fatalf("state = %q after the start, want running", got)
	}

	rebooted := action(t, ir, http.MethodPost, base+"/reboot")
	if rebooted.State != string(instance.StateRunning) {
		t.Errorf("the reboot answered %q, want running", rebooted.State)
	}
	waitForTaskState(t, ir.router, ir.tokens.access, *rebooted.TaskID, "succeeded")
	if got := stateOf(t, ir, launched.ID); got != string(instance.StateRunning) {
		t.Fatalf("state = %q after the reboot, want running: a reboot does not stop anything", got)
	}

	terminated := action(t, ir, http.MethodDelete, base)
	if terminated.State != string(instance.StateTerminating) {
		t.Errorf("the terminate answered %q, want terminating", terminated.State)
	}
	waitForTaskState(t, ir.router, ir.tokens.access, *terminated.TaskID, "succeeded")

	// The terminate does not leave a row behind, so the read that used to answer
	// `terminated` now answers 404 (01-api.md §Instances).
	missing := doWithToken(t, ir.router, http.MethodGet, base, "", ir.tokens.access)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("GET after the terminate status = %d, want 404 (body %s)", missing.Code, missing.Body)
	}

	// Repeating the call is the retry a client does when it never saw the answer.
	// The instance is gone by then, and a DELETE of an unknown id is a 404 rather
	// than a second answer to a question that has been settled.
	again := doWithToken(t, ir.router, http.MethodDelete, base, "", ir.tokens.access)
	if again.Code != http.StatusNotFound {
		t.Fatalf("the repeated DELETE status = %d, want 404 (body %s)", again.Code, again.Body)
	}
}

func TestLifecycleActionsRefuseWhatTheStateDoesNotAllow(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)
	launched := launchedInstance(t, ir, imageID, "web-01")
	base := "/api/v1/instances/" + launched.ID

	// A start of a running instance asks for something that is already true, so
	// it is a conflict rather than a task that would find nothing to do.
	recorder := doWithToken(t, ir.router, http.MethodPost, base+"/start", "", ir.tokens.access)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("a start of a running instance = %d, want 409 (body %s)", recorder.Code, recorder.Body)
	}
	if code := decodeError(t, recorder.Body.Bytes()).Error.Code; code != "conflict" {
		t.Errorf("code = %q, want conflict", code)
	}
	// The message says what the action wanted, so a console can tell the operator
	// what to do instead of only that something is wrong.
	if message := decodeError(t, recorder.Body.Bytes()).Error.Message; !strings.Contains(message, "stopped") {
		t.Errorf("message = %q, want it to name the state the action applies to", message)
	}

	stopped := action(t, ir, http.MethodPost, base+"/stop")
	waitForTaskState(t, ir.router, ir.tokens.access, *stopped.TaskID, "succeeded")

	// A stopped instance takes a start and a terminate, and nothing else: a stop
	// and a reboot are requests for something that is not true any more.
	for _, path := range []string{base + "/stop", base + "/reboot", base + "/stop?force=true"} {
		refused := doWithToken(t, ir.router, http.MethodPost, path, "", ir.tokens.access)
		if refused.Code != http.StatusConflict {
			t.Errorf("POST %s on a stopped instance = %d, want 409 (body %s)",
				path, refused.Code, refused.Body)
		}
	}
}

func TestLifecycleActionsReportAnUnknownInstanceAsNotFound(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)

	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/v1/instances/i-00000000000000000/start"},
		{http.MethodPost, "/api/v1/instances/i-00000000000000000/stop"},
		{http.MethodPost, "/api/v1/instances/i-00000000000000000/reboot"},
		{http.MethodDelete, "/api/v1/instances/i-00000000000000000"},
	} {
		recorder := doWithToken(t, ir.router, test.method, test.path, "", ir.tokens.access)
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s %s status = %d, want 404 (body %s)",
				test.method, test.path, recorder.Code, recorder.Body)
		}
	}

	recorder := doWithToken(t, ir.router, http.MethodGet,
		"/api/v1/instances/i-00000000000000000/console-output", "", ir.tokens.access)
	if recorder.Code != http.StatusNotFound {
		t.Errorf("GET console-output status = %d, want 404 (body %s)", recorder.Code, recorder.Body)
	}
}

func TestASecondActionWhileOneIsInFlightIsAConflict(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)
	launched := launchedInstance(t, ir, imageID, "web-01")
	base := "/api/v1/instances/" + launched.ID

	// The first stop moves the row and queues a task, so the second one fails on
	// one of the three guards: the lock if the worker is already inside the
	// workflow, the state or the unfinished task if it is not. Either way it is a
	// conflict, and never a second task racing the first.
	first := action(t, ir, http.MethodPost, base+"/stop")
	second := doWithToken(t, ir.router, http.MethodPost, base+"/stop", "", ir.tokens.access)
	if second.Code != http.StatusConflict {
		t.Fatalf("the second stop status = %d, want 409 (body %s)", second.Code, second.Body)
	}
	if code := decodeError(t, second.Body.Bytes()).Error.Code; code != "conflict" {
		t.Errorf("code = %q, want conflict", code)
	}

	waitForTaskState(t, ir.router, ir.tokens.access, *first.TaskID, "succeeded")
	if got := stateOf(t, ir, launched.ID); got != string(instance.StateStopped) {
		t.Errorf("state = %q after one stop, want stopped", got)
	}
	// And a stop of a stopped instance is refused too, which is the same guard once
	// the first one is finished.
	third := doWithToken(t, ir.router, http.MethodPost, base+"/stop", "", ir.tokens.access)
	if third.Code != http.StatusConflict {
		t.Errorf("a stop of a stopped instance = %d, want 409 (body %s)", third.Code, third.Body)
	}
}

func TestLifecycleActionsRefuseAViewer(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)
	launched := launchedInstance(t, ir, imageID, "web-01")
	base := "/api/v1/instances/" + launched.ID

	viewer, err := ir.signer.IssueAccessToken("01J0000000000000000000000V", auth.RoleViewer)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}

	for _, test := range []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, base + "/start", ""},
		{http.MethodPost, base + "/stop", ""},
		{http.MethodPost, base + "/stop?force=true", ""},
		{http.MethodPost, base + "/reboot", ""},
		{http.MethodDelete, base, ""},
		{http.MethodPatch, base, `{"name":"renamed"}`},
		{http.MethodGet, base + "/console-output", ""},
		{http.MethodGet, base + "/console", ""},
	} {
		recorder := doWithToken(t, ir.router, test.method, test.path, test.body, viewer.Token)
		if recorder.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s status = %d, want 403 (body %s)",
				test.method, test.path, recorder.Code, recorder.Body)
		}
	}

	// A viewer may still read, and the instance the refusals did not touch is
	// untouched: no lifecycle task was queued behind a 403.
	if got := stateOf(t, ir, launched.ID); got != string(instance.StateRunning) {
		t.Errorf("state = %q after the refusals, want running", got)
	}
	recorder := doWithToken(t, ir.router, http.MethodGet, "/api/v1/tasks?page_size=100", "", viewer.Token)
	var page taskListBody
	decode(t, recorder, &page)
	for _, item := range page.Items {
		if strings.HasPrefix(item.Type, "instance.") && item.Type != "instance.launch" {
			t.Errorf("the refusals queued a %s task", item.Type)
		}
	}
}

// taskListBody is a listing response from GET /tasks.
type taskListBody struct {
	Items []taskBody `json:"items"`
}

func TestForceFlagMustBeABoolean(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)
	launched := launchedInstance(t, ir, imageID, "web-01")
	base := "/api/v1/instances/" + launched.ID

	// A force that is not a boolean must not be read as false: a stop that was
	// meant to be forced would otherwise come back as a graceful request the guest
	// can refuse, and the operator would wait out a watchdog for a shutdown that
	// was never going to happen.
	for _, path := range []string{base + "/stop", base + "/reboot"} {
		for _, query := range []string{"?force=maybe", "?force=1.5", "?force=" + "yes"} {
			recorder := doWithToken(t, ir.router, http.MethodPost, path+query, "", ir.tokens.access)
			if recorder.Code != http.StatusBadRequest {
				t.Errorf("POST %s%s status = %d, want 400 (body %s)",
					path, query, recorder.Code, recorder.Body)
				continue
			}
			envelope := decodeError(t, recorder.Body.Bytes())
			if envelope.Error.Code != "invalid_argument" {
				t.Errorf("code = %q, want invalid_argument", envelope.Error.Code)
			}
			if !hasDetail(envelope, "force") {
				t.Errorf("details = %+v, want one about force", envelope.Error.Details)
			}
		}
	}

	// The values that are booleans are accepted, so the parameter is not simply
	// refused. Only one action can be queued at a time, so the later ones are
	// expected to come back as the conflict of a busy instance rather than as a
	// malformed request.
	for i, value := range []string{"true", "false", "1", "0", "T", "FALSE"} {
		query := "?force=" + value
		recorder := doWithToken(t, ir.router, http.MethodPost, base+"/stop"+query, "", ir.tokens.access)
		switch recorder.Code {
		case http.StatusAccepted:
		case http.StatusConflict:
			// A stop from an earlier line of this loop is still in flight, which
			// says nothing about the parameter.
			if i == 0 {
				t.Errorf("POST %s%s = 409 (body %s), want 202", base, query, recorder.Body)
			}
		default:
			t.Errorf("POST %s%s status = %d, want the parameter accepted (body %s)",
				base, query, recorder.Code, recorder.Body)
		}
	}
}

func TestConsoleOutputServesTheTailOfTheLog(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)
	launched := launchedInstance(t, ir, imageID, "web-01")

	recorder := doWithToken(t, ir.router, http.MethodGet,
		"/api/v1/instances/"+launched.ID+"/console-output", "", ir.tokens.access)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}

	var out consoleBody
	decode(t, recorder, &out)
	if out.InstanceID != launched.ID {
		t.Errorf("instance_id = %q, want %q", out.InstanceID, launched.ID)
	}
	if !strings.Contains(out.Output, "booting") {
		t.Errorf("the output does not carry the boot line: %q", out.Output)
	}

	// The tail is bounded, so a caller cannot ask for a log the size of the disk.
	tailed := doWithToken(t, ir.router, http.MethodGet,
		"/api/v1/instances/"+launched.ID+"/console-output?tail_kib=1", "", ir.tokens.access)
	if tailed.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", tailed.Code, tailed.Body)
	}
	var tail consoleBody
	decode(t, tailed, &tail)
	if len(tail.Output) > 1<<10 {
		t.Errorf("the tail is %d bytes, want at most 1024", len(tail.Output))
	}

	for _, query := range []string{
		"?tail_kib=0",
		"?tail_kib=-1",
		fmt.Sprintf("?tail_kib=%d", instance.MaxConsoleTailKiB+1),
		"?tail_kib=lots",
		"?tail_kib=",
	} {
		recorder := doWithToken(t, ir.router, http.MethodGet,
			"/api/v1/instances/"+launched.ID+"/console-output"+query, "", ir.tokens.access)
		// An absent parameter takes the default and an empty one is the same as
		// absent, so only the values that are not a number in range are refused.
		if query == "?tail_kib=" {
			if recorder.Code != http.StatusOK {
				t.Errorf("an empty tail_kib = %d, want the default 200", recorder.Code)
			}
			continue
		}
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400 (body %s)", query, recorder.Code, recorder.Body)
			continue
		}
		if !hasDetail(decodeError(t, recorder.Body.Bytes()), "tail_kib") {
			t.Errorf("%s details, want one about tail_kib", query)
		}
	}
}

func TestInstanceConsoleIsNotBuiltYet(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)
	launched := launchedInstance(t, ir, imageID, "web-01")

	recorder := doWithToken(t, ir.router, http.MethodGet,
		"/api/v1/instances/"+launched.ID+"/console", "", ir.tokens.access)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body %s)", recorder.Code, recorder.Body)
	}
	if code := decodeError(t, recorder.Body.Bytes()).Error.Code; code != "unavailable" {
		t.Errorf("code = %q, want unavailable", code)
	}

	// An instance that is not there is a 404 rather than the 501 of a console that
	// is not built: a client that cannot tell the two answers apart cannot recover
	// from either.
	missing := doWithToken(t, ir.router, http.MethodGet,
		"/api/v1/instances/i-00000000000000000/console", "", ir.tokens.access)
	if missing.Code != http.StatusNotFound {
		t.Errorf("GET /console on an unknown instance = %d, want 404 (body %s)",
			missing.Code, missing.Body)
	}
}

func TestUpdateInstanceRenamesAndRetags(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)
	launched := launchedInstance(t, ir, imageID, "web-01")
	base := "/api/v1/instances/" + launched.ID

	recorder := doWithToken(t, ir.router, http.MethodPatch, base,
		`{"name":"web-02","tags":{"env":"staging","team":"platform"}}`, ir.tokens.access)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PATCH /instances/{id} status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}
	var renamed instanceBody
	decode(t, recorder, &renamed)
	if renamed.Name != "web-02" {
		t.Errorf("name = %q, want web-02", renamed.Name)
	}
	if renamed.Tags["env"] != "staging" || renamed.Tags["team"] != "platform" {
		t.Errorf("tags = %v, want the ones from the body", renamed.Tags)
	}
	if renamed.ID != launched.ID {
		t.Errorf("id = %q, want %q: a rename is not a new instance", renamed.ID, launched.ID)
	}
	// A rename touches nothing else: the launch inputs and the runtime data are
	// what the domain is doing, not what the name says.
	if renamed.ImageID != launched.ImageID || renamed.State != string(instance.StateRunning) {
		t.Errorf("the patch changed the instance: image %q, state %q",
			renamed.ImageID, renamed.State)
	}

	// A tag set replaces the tags: the console sends the whole set, and a merge
	// would leave a tag behind that the operator deleted from the form.
	replaced := doWithToken(t, ir.router, http.MethodPatch, base,
		`{"tags":{"env":"prod"}}`, ir.tokens.access)
	if replaced.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", replaced.Code, replaced.Body)
	}
	var retagged instanceBody
	decode(t, replaced, &retagged)
	if len(retagged.Tags) != 1 || retagged.Tags["env"] != "prod" {
		t.Errorf("tags = %v, want just env=prod", retagged.Tags)
	}
	// A patch that says nothing about the name leaves it alone.
	if retagged.Name != "web-02" {
		t.Errorf("name = %q, want the rename to survive a tag patch", retagged.Name)
	}

	// An empty tag set is how every tag goes, which is not the same as saying
	// nothing about the tags.
	cleared := doWithToken(t, ir.router, http.MethodPatch, base, `{"tags":{}}`, ir.tokens.access)
	if cleared.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", cleared.Code, cleared.Body)
	}
	var empty instanceBody
	decode(t, cleared, &empty)
	if len(empty.Tags) != 0 {
		t.Errorf("tags = %v, want none", empty.Tags)
	}
}

func TestUpdateInstanceRefusesWhatItCannotDo(t *testing.T) {
	t.Parallel()

	ir := newInstanceRouter(t)
	imageID := availableImageID(t, ir)
	first := launchedInstance(t, ir, imageID, "web-01")
	second := launchedInstance(t, ir, imageID, "web-02")
	base := "/api/v1/instances/" + first.ID

	for _, test := range []struct {
		name  string
		body  string
		field string
	}{
		{"an invalid name", `{"name":"Not A Label"}`, "name"},
		{"an empty name", `{"name":""}`, "name"},
		{"an empty tag key", `{"tags":{"":"value"}}`, "tags"},
		{"an overlong tag", `{"tags":{"key":"` + strings.Repeat("v", 257) + `"}}`, "tags"},
		{"nothing at all", `{}`, "body"},
		{"a body that is not an object", `[]`, "body"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := doWithToken(t, ir.router, http.MethodPatch, base, test.body, ir.tokens.access)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body)
			}
			if !hasDetail(decodeError(t, recorder.Body.Bytes()), test.field) {
				t.Errorf("details = %+v, want one about %q",
					decodeError(t, recorder.Body.Bytes()).Error.Details, test.field)
			}
		})
	}

	// A name that is in use is a conflict rather than a malformed request, and the
	// instance keeps the name it had.
	taken := doWithToken(t, ir.router, http.MethodPatch, base, `{"name":"web-02"}`, ir.tokens.access)
	if taken.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", taken.Code, taken.Body)
	}
	if got := stateOf(t, ir, first.ID); got != string(instance.StateRunning) {
		t.Errorf("state = %q, want running", got)
	}
	detail := doWithToken(t, ir.router, http.MethodGet, base, "", ir.tokens.access)
	var found instanceBody
	decode(t, detail, &found)
	if found.Name != "web-01" {
		t.Errorf("name = %q after the refused rename, want web-01", found.Name)
	}

	missing := doWithToken(t, ir.router, http.MethodPatch,
		"/api/v1/instances/i-00000000000000000", `{"name":"web-03"}`, ir.tokens.access)
	if missing.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (body %s)", missing.Code, missing.Body)
	}

	// The instance the rename refused to steal is untouched too.
	if got := stateOf(t, ir, second.ID); got != string(instance.StateRunning) {
		t.Errorf("the other instance is %q, want running", got)
	}
}

package progress

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"moul.io/u"
)

// Progress is the top-level object of the 'progress' library.
type Progress struct {
	Steps     []*Step   `json:"steps,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`

	mutex       sync.RWMutex
	subscribers []chan *Step
}

type State string

const (
	StateNotStarted State = "not started"
	StateInProgress State = "in progress"
	StateDone       State = "done"
)

// New creates and returns a new Progress.
func New() *Progress {
	return &Progress{
		CreatedAt: time.Now(),
	}
}

// AddStep creates and returns a new Step with the provided 'id'.
// A non-empty, unique 'id' is required, else it will panic.
func (p *Progress) AddStep(id string) *Step {
	step, err := p.SafeAddStep(id)
	if err != nil {
		panic(err)
	}
	return step
}

// SafeAddStep is equivalent to AddStep with but returns error instead of panicking.
func (p *Progress) SafeAddStep(id string) (*Step, error) {
	if id == "" {
		return nil, ErrStepRequiresID
	}
	step := &Step{
		ID:     id,
		State:  StateNotStarted,
		parent: p,
	}

	p.mutex.Lock()
	defer p.mutex.Unlock()
	if p.Steps == nil {
		p.Steps = make([]*Step, 0)
	}

	for _, step := range p.Steps {
		if step.ID == id {
			return nil, ErrStepIDShouldBeUnique
		}
	}

	p.Steps = append(p.Steps, step)
	p.publishStep(step)
	return step, nil
}

// publishStep iterates over subscribers and try to append a step.
func (p *Progress) publishStep(step *Step) {
	var stepCopyPtr *Step
	if step != nil {
		stepCopy := *step
		stepCopyPtr = &stepCopy
	}
	for _, subscriber := range p.subscribers {
		subscriber <- stepCopyPtr
	}
}

// Subscribe register a provided chan as a target called each time a step is changed.
func (p *Progress) Subscribe(subscriber chan *Step) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if p.subscribers == nil {
		p.subscribers = make([]chan *Step, 0)
	}

	p.subscribers = append(p.subscribers, subscriber)
}

// Get retrieves a Step by its 'id'.
// A non-empty 'id' is required, else it will panic.
// If 'id' does not match an existing step, nil is returned.
func (p *Progress) Get(id string) *Step {
	if id == "" {
		panic("progress.Get requires a non-empty ID as argument.")
	}

	p.mutex.RLock()
	defer p.mutex.RUnlock()

	for _, step := range p.Steps {
		if step.ID == id {
			return step
		}
	}

	return nil
}

// Snapshot represents info and stats about a progress at a given time.
type Snapshot struct {
	State              State         `json:"state,omitempty"`
	Doing              string        `json:"doing,omitempty"`
	NotStarted         int           `json:"not_started,omitempty"`
	InProgress         int           `json:"in_progress,omitempty"`
	Completed          int           `json:"completed,omitempty"`
	Total              int           `json:"total,omitempty"`
	Percent            float64       `json:"percent,omitempty"`
	TotalDuration      time.Duration `json:"total_duration,omitempty"`
	StepDuration       time.Duration `json:"step_duration,omitempty"`
	CompletionEstimate time.Duration `json:"completion_estimate,omitempty"`
	DoneAt             *time.Time    `json:"done_at,omitempty"`
	StartedAt          *time.Time    `json:"started_at,omitempty"`
}

// Snapshot computes and returns the current stats of the Progress.
func (p *Progress) Snapshot() Snapshot {
	p.mutex.RLock()
	defer p.mutex.RUnlock()
	if len(p.Steps) == 0 {
		return Snapshot{
			State: StateNotStarted,
		}
	}

	snapshot := Snapshot{
		Total:   len(p.Steps),
		Percent: 0,
	}

	doing := []string{}
	for _, step := range p.Steps {
		switch step.State {
		case StateNotStarted:
			snapshot.NotStarted++
		case StateInProgress:
			snapshot.InProgress++
			doing = append(doing, step.title())
		case StateDone:
			snapshot.Completed++
		default:
			panic(fmt.Sprintf("step is in an unexpected state: %s", u.JSON(step)))
		}

		// compute the oldest step.StartedAt
		if step.StartedAt != nil {
			if snapshot.StartedAt == nil {
				snapshot.StartedAt = step.StartedAt
			} else if step.StartedAt.Before(*snapshot.StartedAt) {
				snapshot.StartedAt = step.StartedAt
			}
		}

		// compute the most recent step.DoneAt
		if step.DoneAt != nil {
			if snapshot.DoneAt == nil {
				snapshot.DoneAt = step.DoneAt
			} else if step.DoneAt.After(*snapshot.DoneAt) {
				snapshot.DoneAt = step.DoneAt
			}
		}
	}

	snapshot.Percent = p.Percent()

	// compute top-level aggregates
	{
		snapshot.Doing = strings.Join(doing, ", ")
		var (
			isDone       = snapshot.Completed > 0 && snapshot.InProgress == 0 && snapshot.NotStarted == 0
			isInProgress = snapshot.Completed < snapshot.Total && (snapshot.InProgress > 0 || snapshot.Completed > 0)
			isNotStarted = snapshot.Completed == 0 && snapshot.InProgress == 0
		)
		switch {
		case isDone:
			snapshot.State = StateDone
			if snapshot.Completed != snapshot.Total {
				panic(fmt.Sprintf("snapshot has a strange state: %s", u.JSON(snapshot)))
			}
			snapshot.Percent = 100 // avoid having 99.99999999999
			snapshot.TotalDuration = snapshot.DoneAt.Sub(*snapshot.StartedAt)
		case isInProgress:
			snapshot.State = StateInProgress
			snapshot.DoneAt = nil
			snapshot.TotalDuration = time.Since(*snapshot.StartedAt)
		case isNotStarted:
			snapshot.State = StateNotStarted
			snapshot.DoneAt = nil
		default:
			panic(fmt.Sprintf("snapshot has a strange state: %s", u.JSON(snapshot)))
		}
	}

	return snapshot
}

// MarshalJSON is a custom JSON marshaler that automatically computes and append the current snapshot.
func (p *Progress) MarshalJSON() ([]byte, error) {
	type alias Progress
	type enriched struct {
		*alias
		Snapshot Snapshot `json:"snapshot"`
	}
	return json.Marshal(&enriched{
		alias:    (*alias)(p),
		Snapshot: p.Snapshot(),
	})
}

// Percent returns the current completion percentage, it's a faster alternative to Progress.Snapshot().Percent.
func (p *Progress) Percent() float64 {
	total := len(p.Steps)
	percent := float64(0)
	for _, step := range p.Steps {
		switch step.State {
		case StateNotStarted:
			// noop
		case StateInProgress:
			// in-progress task count as partially done
			percent += (float64(0.5) / float64(total)) * 100 // nolint:gomnd
			// FIXME: support per-task progress
		case StateDone:
			percent += (float64(1) / float64(total)) * 100 // nolint:gomnd
		default:
			panic(fmt.Sprintf("step is in an unexpected state: %s", u.JSON(step)))
		}
	}
	return percent
}

func (p *Progress) isDone() bool {
	if len(p.Steps) == 0 {
		return false
	}
	for _, step := range p.Steps {
		if step.State != StateDone {
			return false
		}
	}
	return true
}

// Step represents a progress step.
// It always have an 'id' and can be customized using helpers.
type Step struct {
	ID          string      `json:"id,omitempty"`
	Description string      `json:"description,omitempty"`
	StartedAt   *time.Time  `json:"started_at,omitempty"`
	DoneAt      *time.Time  `json:"done_at,omitempty"`
	State       State       `json:"state,omitempty"`
	Data        interface{} `json:"data,omitempty"`

	parent *Progress
}

// SetDescription sets a custom step description.
// It returns itself (*Step) for chaining.
func (s *Step) SetDescription(desc string) *Step {
	s.Description = desc
	s.parent.publishStep(s)
	return s
}

// SetData sets a custom step data.
// It returns itself (*Step) for chaining.
func (s *Step) SetData(data interface{}) *Step {
	s.Data = data
	s.parent.publishStep(s)
	return s
}

// Start marks a step as started.
// If a step was already InProgress or Done, it panics.
func (s *Step) Start() {
	s.parent.mutex.Lock()
	defer s.parent.mutex.Unlock()
	if s.State == StateInProgress {
		panic("cannot Step.Start() an already in-progress step.")
	}
	if s.State == StateDone {
		panic("cannot Step.Start() an already done step.")
	}
	s.State = StateInProgress
	now := time.Now()
	s.StartedAt = &now
	s.parent.publishStep(s)
}

// SetAsCurrent stops all in-progress steps and start this one.
func (s *Step) SetAsCurrent() {
	s.parent.mutex.Lock()
	defer s.parent.mutex.Unlock()
	if s.State == StateInProgress {
		panic("cannot Step.Start() an already in-progress step.")
	}
	if s.State == StateDone {
		panic("cannot Step.Start() an already done step.")
	}
	now := time.Now()
	for _, step := range s.parent.Steps {
		if step.State == StateInProgress {
			step.State = StateDone
			step.DoneAt = &now
			s.parent.publishStep(step)
		}
	}
	s.State = StateInProgress
	s.StartedAt = &now
	s.parent.publishStep(s)
}

// Done marks a step as done.
// If the step was already done, it panics.
func (s *Step) Done() {
	s.parent.mutex.Lock()
	defer s.parent.mutex.Unlock()
	if s.State == StateDone {
		panic("cannot Step.Done() an already done step.")
	}
	s.State = StateDone
	now := time.Now()
	if s.StartedAt == nil {
		s.StartedAt = &now
	}
	s.DoneAt = &now
	s.parent.publishStep(s)
	if s.parent.isDone() {
		s.parent.publishStep(nil)
	}
}

// MarshalJSON is a custom JSON marshaler that automatically computes and append some runtime metadata.
func (s *Step) MarshalJSON() ([]byte, error) {
	type alias Step
	type enriched struct {
		alias
		Duration time.Duration `json:"duration,omitempty"`
	}
	return json.Marshal(&enriched{
		alias:    (alias)(*s),
		Duration: s.Duration(),
	})
}

// Duration computes the step duration.
func (s *Step) Duration() time.Duration {
	var ret time.Duration
	switch s.State {
	case StateInProgress:
		ret = time.Since(*s.StartedAt)
	case StateDone:
		ret = s.DoneAt.Sub(*s.StartedAt)
	case StateNotStarted:
		// noop
	default:
		// noop
	}
	return ret
}

func (s *Step) title() string {
	if s.Description != "" {
		return s.Description
	}
	return s.ID
}

var (
	ErrStepRequiresID       = errors.New("progress.AddStep requires a non-empty ID as argument")
	ErrStepIDShouldBeUnique = errors.New("progress.AddStep requires a unique ID as argument")
)

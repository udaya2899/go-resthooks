package resthooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	// start retrying after 5 seconds and
	// grow exponentially after that:
	// 1st retry = after 5 seconds
	// 2nd retry = 5*3 = after 15 seconds
	// 3rd retry (final) = 5*3*3 = after 45 seconds
	defaultInitialRetry    int = 5
	defaultRetryMultiplier int = 3

	// maximum number of attempts
	defaultMaxRetry int = 3
)

type Config struct {
	InitialRetry    int
	RetryMultiplier int
	MaxRetry        int
}

type Resthook struct {
	config Config
	store  ResthookStore
	result chan *Notification
}

func NewResthook(store ResthookStore, config ...Config) Resthook {
	rh := Resthook{
		config: Config{
			InitialRetry:    defaultInitialRetry,
			RetryMultiplier: defaultRetryMultiplier,
			MaxRetry:        defaultMaxRetry,
		},
		store:  store,
		result: make(chan *Notification),
	}

	if len(config) > 0 {
		rh.config = config[0]
	}

	go func() {
		for {
			select {

			// if channel is closed, exit goroutine
			case _, ok := <-rh.result:
				if !ok {
					return
				}

			// if we don't have any result,
			// block for 1sec and then loop
			case <-time.After(1 * time.Second):
			}
		}
	}()

	return rh
}

func (rh Resthook) GetResults() <-chan *Notification {
	return rh.result
}

func (rh Resthook) Close() {
	close(rh.result)
}

func (rh Resthook) Handler() http.Handler {
	return NewHandler(&rh)
}

func (rh Resthook) Save(s *Subscription) error {
	return rh.store.Save(s)
}

func (rh Resthook) FindById(id int) (*Subscription, error) {
	return rh.store.FindById(id)
}

func (rh Resthook) DeleteById(id int) error {
	// validate that subscription actually exists
	s, err := rh.FindById(id)
	if err != nil {
		return err
	}

	if s == nil {
		return errors.New("Invalid subscription.")
	}

	return rh.store.DeleteById(s.Id)
}

func (rh Resthook) Notify(userId int, event string, data interface{}) error {
	s, err := rh.store.FindByUserId(userId, event)
	if err != nil {
		return err
	}

	out, err := json.Marshal(data)
	if err != nil {
		return err
	}

	n := &Notification{
		Subscription: s,
		Data:         out,
		Status:       STATUS_PENDING,
	}
	resp, err := http.Post(s.TargetUrl, "application/json", n.asReader())
	if err == nil && resp.StatusCode < 300 {
		n.Status = STATUS_SUCCESS
		rh.result <- n
		return nil
	}

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		n.Status = STATUS_FAILED
		rh.result <- n
		return fmt.Errorf("Unable to notify: %d", resp.StatusCode)
	}

	if resp.StatusCode == 410 {
		n.Status = STATUS_FAILED
		rh.result <- n
		return rh.DeleteById(s.Id)
	}

	// otherwise we retry
	go rh.retry(n)

	return nil
}

func (rh *Resthook) retry(n *Notification) {
	interval := rh.config.InitialRetry
	for {
		select {

		// all retry goroutines should close on resthook close
		case _, ok := <-rh.result:
			if !ok {
				return
			}

		case <-time.After(time.Duration(interval) * time.Second):
			n.Retries += 1
			resp, err := http.Post(n.Subscription.TargetUrl, "application/json", n.asReader())
			if err == nil && resp.StatusCode < 300 {
				n.Status = STATUS_SUCCESS
				rh.result <- n
				return
			}

			// terminate after max attempts
			interval *= rh.config.RetryMultiplier
			if n.Retries == rh.config.MaxRetry {
				n.Status = STATUS_FAILED
				rh.result <- n
				return
			}
		}
	}
}
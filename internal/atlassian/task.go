package atlassian

import (
	"context"
	"fmt"
	"time"
)

// taskPollInterval and taskTimeout bound WaitForTask. Tests shorten the interval.
var (
	taskPollInterval = 2 * time.Second
	taskTimeout      = 10 * time.Minute
)

// taskResponse is the part of GET /rest/api/3/task/{taskId} that WaitForTask reads.
type taskResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// WaitForTask polls a Jira asynchronous task until it ends. It returns nil
// when the task completes, and an error when it fails, is cancelled or does
// not end within taskTimeout.
func (c *Client) WaitForTask(ctx context.Context, taskID string) error {
	taskPath := "/rest/api/3/task/" + PathEscape(taskID)
	deadline := time.Now().Add(taskTimeout)
	for {
		var task taskResponse
		if err := c.Get(ctx, taskPath, &task); err != nil {
			return fmt.Errorf("polling task %s: %w", taskID, err)
		}

		switch task.Status {
		case "COMPLETE":
			return nil
		case "ENQUEUED", "RUNNING":
		default:
			// FAILED, CANCEL_REQUESTED, CANCELLED or DEAD.
			return fmt.Errorf("task %s ended with status %s: %s", taskID, task.Status, task.Message)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("task %s did not finish within %s (status %s)", taskID, taskTimeout, task.Status)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(taskPollInterval):
		}
	}
}

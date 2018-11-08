package main

import (
	"fmt"
	"time"

	"google.golang.org/api/compute/v1"
)

func (c *Controller) waitForOperation(op *compute.Operation) (err error) {
	for {
		if op.Status == "DONE" {
			return nil
		}

		time.Sleep(1000 * time.Millisecond)

		op, err = c.computeService.GlobalOperations.Get(c.project, op.Name).Do()
		if err != nil {
			return err
		}
	}
}

func (c *Controller) backendServiceFDN(name string) string {
	return fmt.Sprintf("projects/%s/global/backendServices/%s", c.project, name)
}

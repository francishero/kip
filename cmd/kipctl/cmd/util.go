/*
Copyright 2020 Elotl Inc.

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

package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/elotl/cloud-instance-provider/pkg/clientapi"
)

func fatal(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	fmt.Fprint(os.Stderr, msg)
	os.Exit(1)
}

func dieIfReplyError(cmd string, reply *clientapi.APIReply) {
	if reply.Status < 200 || reply.Status >= 400 {
		fatal("%s returned %d - %s", cmd, reply.Status, reply.Body)
	}
}

func dieIfError(err error, format string, args ...interface{}) {
	if err != nil {
		s := fmt.Sprintf(format, args...)
		msg := s + ": " + err.Error()
		fatal(msg)
	}
}
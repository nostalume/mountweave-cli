package main

import (
	"crypto/sha256"
	"encoding/json"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/nostalume/mountweave/browser"
)

// diagnosticForm keeps file metadata and operation switches. Other form fields
// may carry credentials or whole uploads and stay out of the report.
func diagnosticForm(body []byte) (json.RawMessage, string) {
	values, err := url.ParseQuery(string(body))
	if err != nil || len(values) > 64 {
		return nil, ""
	}
	fields := make(map[string]any)
	for key, entries := range values {
		if len(entries) != 1 {
			continue
		}
		switch diagnosticKey(key) {
		case "filelist", "taskinfos":
			if payload := diagnosticJSON([]byte(entries[0])); payload != nil {
				fields[key] = payload
			}
		case "fid", "fids", "fileid", "fileids", "folderid", "parentfileid",
			"pid", "cid", "path", "dest", "newname", "destfilename", "filename",
			"foldername", "title", "name", "type", "task", "opera", "operation",
			"action", "isdir", "onnest", "ondup", "filerenamemode", "failifnameexist",
			"actiononduplicate":
			if !diagnosticSensitiveText(entries[0]) && !diagnosticOpaqueValue(entries[0]) {
				fields[key] = entries[0]
			}
		}
	}
	task := singleDiagnosticValue(values, "taskid")
	if len(fields) == 0 {
		return nil, task
	}
	projected, err := json.Marshal(fields)
	if err != nil || len(projected) > maxDiagnosticBodyBytes {
		return nil, task
	}
	return projected, task
}

func singleDiagnosticValue(values url.Values, key string) string {
	var match string
	found := false
	for name, entries := range values {
		if diagnosticKey(name) != key {
			continue
		}
		if len(entries) != 1 || found {
			return ""
		}
		match, found = entries[0], true
	}
	return match
}

type taskEvidence struct {
	Number int    `json:"number"`
	State  string `json:"state,omitempty"`
	Errno  *int   `json:"task_errno,omitempty"`
}

func diagnosticTaskKey(endpoint, id string) [sha256.Size]byte {
	if id == "" || id == "0" || len(id) > 128 {
		return [sha256.Size]byte{}
	}
	for _, r := range id {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r == '_' || r == '-') {
			return [sha256.Size]byte{}
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil || requestOrigin(u) == "" {
		return [sha256.Size]byte{}
	}
	return sha256.Sum256([]byte(requestOrigin(u) + "\x00" + id))
}

// Only known state words and bounded numeric states are surfaced. A general
// HTTP or business success code does not establish a task's final state.
func diagnosticTaskJSON(body json.RawMessage) (id, state string, taskErrno *int) {
	var data map[string]json.RawMessage
	if json.Unmarshal(body, &data) != nil {
		return "", "", nil
	}
	// Some services return the task ID in data and the terminal state in data.taskInfo.
	for depth := 0; data != nil && depth < 3; depth++ {
		var status, taskStatus string
		var seenID bool
		for key, value := range data {
			switch diagnosticKey(key) {
			case "taskid":
				candidate := diagnosticScalar(value)
				if seenID || candidate != "" && id != "" && candidate != id {
					return "", "", nil
				}
				seenID = true
				if candidate != "" {
					id = candidate
				}
			case "status":
				status = diagnosticScalar(value)
			case "taskstatus":
				taskStatus = diagnosticScalar(value)
			case "taskerrno":
				if n, err := strconv.Atoi(diagnosticScalar(value)); err == nil && n >= -1000000 && n <= 1000000 {
					taskErrno = new(n)
				}
			}
		}
		if taskStatus != "" {
			state = taskStatus
		} else if status != "" {
			state = status
		}
		next := data["taskInfo"]
		if len(next) == 0 {
			next = data["data"]
		}
		data = nil
		_ = json.Unmarshal(next, &data)
	}
	switch state {
	case "success", "succeeded", "failed", "fail", "running", "pending", "done", "canceled", "cancelled",
		"partial_success", "Succeed", "Failed", "Running", "PartialSucceed":
		return id, state, taskErrno
	}
	n, err := strconv.Atoi(state)
	if err == nil && n >= -10 && n <= 99 {
		return id, strconv.Itoa(n), taskErrno
	}
	return id, "", taskErrno
}

func diagnosticScalar(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] != '"' {
		return string(raw)
	}
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func linkTasks(reports []networkReport) {
	numbers := make(map[[sha256.Size]byte]int)
	var empty [sha256.Size]byte
	for i := range reports {
		for j := range reports[i].Requests {
			request := &reports[i].Requests[j]
			id, state, taskErrno := diagnosticTaskJSON(request.ResponseJSON)
			replyKey := diagnosticTaskKey(request.Endpoint, id)
			key := request.taskKey
			if key == empty {
				key = replyKey
			}
			if key == empty || replyKey != empty && replyKey != key {
				continue
			}
			numbers[key] = alias(numbers, key)
			request.Task = &taskEvidence{Number: numbers[key], State: state, Errno: taskErrno}
		}
	}
}

func diagnosticOperation(value string) bool {
	switch strings.ToLower(value) {
	case "rename", "move", "delete", "trash", "recycle", "create", "mkdir", "copy", "upload", "list":
		return true
	default:
		return false
	}
}
func diagnosticMutationRoute(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	last := path.Base(u.Path)
	key := diagnosticKey(strings.TrimSuffix(last, path.Ext(last)))
	return key != "list" && diagnosticOperation(key) || strings.Contains(key, "task") || strings.Contains(key, "recycle") ||
		strings.HasPrefix(key, "rename") || strings.HasPrefix(key, "create") || strings.Contains(key, "delete") ||
		browser.KnownMutationReceiptRoute(u)
}

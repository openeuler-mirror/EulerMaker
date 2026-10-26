//go:build ignore

package main

import (
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	ebsv1 "ebs-api/ebs/v1"
)

var bearer = []object{{"BearerAuth": []string{}}}
var optionalBearer = []object{{}, {"BearerAuth": []string{}}}

func operations() object {
	paths := object{}
	auth := func(method, path, id, summary, description, input, output, code string, security []object) {
		op := operation(id, "Authentication", summary, description, security, ref(output), code)
		if input != "" {
			op["requestBody"] = body(ref(input), true)
		}
		if code == "204" {
			op["responses"].(object)[code] = object{"description": "Password changed; no response body."}
		}
		add(paths, path, method, op)
	}
	auth("post", "/auth/register", "registerUser", "Register a user", "Anonymous registration; returns the username, not a token.", "RegisterRequest", "RegisteredUser", "201", []object{})
	auth("post", "/auth/login", "login", "Obtain a user token", "Anonymous login. Response must not be cached.", "LoginRequest", "LoginResponse", "200", []object{})
	auth("post", "/auth/runner-token", "obtainRunnerToken", "Obtain a Runner token", "MachineAccount Basic credentials and the Runner name are required.", "RunnerTokenRequest", "RunnerTokenResponse", "200", []object{{"MachineBasic": []string{}}})
	auth("post", "/auth/check", "checkToken", "Check a token", "Accepts user and Runner Bearer tokens. No request body.", "", "CheckResponse", "200", bearer)
	auth("put", "/auth/users/{name}/password", "changePassword", "Change your password", "Only the user named in the path may change their password; current password is required.", "PasswordRequest", "", "204", bearer)
	auth("post", "/auth/machineaccounts", "registerMachineAccount", "Register a machine account", "Admin only. The secret is supplied by the caller and is never returned.", "MachineRequest", "RegisteredMachine", "201", bearer)
	for _, r := range []struct{ name, kind string }{{"users", "User"}, {"machineaccounts", "MachineAccount"}} {
		base := "/apis/iam.ebs/v1/" + r.name
		for _, named := range []bool{false, true} {
			path, kind := base, r.kind+"List"
			methods := []string{"get", "head"}
			if named {
				path, kind = base+"/{name}", r.kind
				methods = append(methods, "delete")
				if r.name == "users" {
					methods = append(methods, "put", "patch")
				}
			}
			for _, method := range methods {
				op := operation(method+r.kind+map[bool]string{false: "List", true: ""}[named], "IAM", method+" "+kind,
					"Admin only. Admin User accounts are excluded from management. User updates may change permitted profile/scope fields; credentials are managed through /auth. Machine accounts are read/delete only.",
					bearer, ref("ebs-api/iam/v1."+kind), "200")
				setResourceRequest(op, method, ref("ebs-api/iam/v1."+r.kind), false)
				if method == "put" {
					op["requestBody"] = body(ref("UserUpdate"), true)
				}
				if (method == "get" || method == "head") && r.name != "users" {
					op["parameters"] = queryParameters(!named, false, true)
				}
				if r.name == "users" && !named {
					op["parameters"] = queryParameters(true, false, true)[2:]
				}
				describeFieldSelector(op, r.kind)
				add(paths, path, method, op)
			}
		}
	}
	for _, r := range []struct {
		name, kind       string
		scoped, readOnly bool
	}{
		{"projects", "Project", false, false}, {"runners", "Runner", false, false},
		{"configs", "Config", false, false}, {"scripts", "Script", false, false},
		{"builds", "Build", true, false}, {"jobs", "Job", true, false},
		{"snapshots", "Snapshot", true, true}, {"buildinfos", "BuildInfo", true, true},
		{"buildgraphs", "BuildGraph", true, true}, {"rpmrepos", "RpmRepo", true, true},
		{"oldjobs", "Job", true, true},
	} {
		base := "/apis/ebs/v1/"
		if r.scoped {
			base += "projects/{project}/"
		}
		base += r.name
		for _, part := range []string{"list", "object", "status", "abort"} {
			if part == "status" && (r.name == "configs" || r.name == "scripts" || r.name == "oldjobs") {
				continue
			}
			if part == "abort" && r.name != "builds" && r.name != "jobs" {
				continue
			}
			path, responseKind := base, r.kind+"List"
			if part != "list" {
				nameParam := "name"
				if r.name == "projects" {
					nameParam = "project"
				}
				if r.name == "runners" {
					nameParam = "runner"
				}
				path, responseKind = base+"/{"+nameParam+"}", r.kind
				if part != "object" {
					path += "/" + part
				}
			}
			methods := []string{"get"}
			if !r.readOnly {
				if r.name != "scripts" {
					methods = append(methods, "head")
				}
				switch part {
				case "list":
					methods = append(methods, "post")
				case "object", "status":
					if r.name != "builds" {
						methods = append(methods, "put", "patch")
					}
					if part == "object" {
						methods = append(methods, "delete")
					}
				case "abort":
					methods = []string{"post"}
				}
			}
			for _, method := range methods {
				security, description := permissions(r.name, part, method)
				code := "200"
				if method == "post" && part == "list" {
					code = "201"
				}
				op := operation(method+strings.Title(r.name)+strings.Title(part), r.kind, method+" "+r.name+" "+part,
					description, security, resourceRef(responseKind), code)
				if method == "get" || method == "head" {
					op["parameters"] = queryParameters(part == "list", r.name == "jobs" && part == "list" && method == "get", true)
					if r.name == "configs" && part == "object" {
						delete(op, "parameters")
					}
					if r.name == "jobs" && part == "list" && method == "get" {
						watchResponse(op)
					}
				}
				setResourceRequest(op, method, resourceRef(r.kind), part == "abort")
				describeFieldSelector(op, r.kind)
				if part != "abort" && (method == "post" || method == "put") {
					mode := "Update"
					if method == "post" {
						mode = "Create"
					}
					if part == "status" {
						mode = "StatusUpdate"
					}
					op["requestBody"] = body(ref(r.kind+mode), true)
				}
				if part == "abort" && r.name == "jobs" {
					op["requestBody"] = body(ref("JobAbort"), true)
				}
				add(paths, path, method, op)
			}
		}
	}
	op := operation("getRunnerJobs", "Runner", "List or watch assigned Jobs",
		"Runner may read only its own assignments; Ops/Admin may read the list. Server fixes the Runner filter. "+
			"Watch requires authentication and uses newline-delimited Kubernetes WatchEvent JSON (ADDED/MODIFIED/DELETED/BOOKMARK/ERROR). "+
			"Runner watch permits one connection per Runner, timeoutSeconds 1–300. Resume with resourceVersion; restart List after 410.",
		bearer, resourceRef("JobList"), "200")
	op["parameters"] = queryParameters(true, true, false)
	watchResponse(op)
	add(paths, "/apis/ebs/v1/runners/{runner}/jobs", "get", op)
	add(paths, "/apis/ebs/v1/service-endpoints", "get", operation("getServiceEndpoints", "Runner", "Get Runner service addresses",
		"Runner token only. Returns all configured Runner endpoints, not management internalURL. No query parameters.", bearer, ref("ServiceEndpoints"), "200"))
	return paths
}

func operation(id, tag, summary, description string, security []object, schema object, code string) object {
	responses := object{code: object{"description": "Successful response.", "content": object{"application/json": object{"schema": schema}}}}
	for status := range errorResponses() {
		responses[status] = object{"$ref": "#/components/responses/" + status}
	}
	return object{"operationId": id, "tags": []string{tag}, "summary": summary, "description": description,
		"security": security, "responses": responses}
}

func errorResponses() object {
	responses := object{}
	for _, item := range []struct{ code, description string }{
		{"400", "Malformed request."}, {"401", "Missing, invalid or expired credentials."},
		{"403", "Operation not permitted for this identity or object."}, {"404", "Resource not found."},
		{"409", "Already exists or UID/resourceVersion/state conflict; read the latest object before retrying."},
		{"410", "Expired continue token or watch resourceVersion; restart List."},
		{"413", "Request body too large."}, {"422", "Resource validation failed."},
		{"415", "Unsupported request content type."},
		{"429", "Rate or watch connection limit exceeded; honor Retry-After when present."},
		{"500", "Server failure; write outcome may be unknown."}, {"502", "Upstream failure; write outcome may be unknown."},
		{"503", "Service temporarily unavailable."}, {"504", "Upstream timeout; write outcome may be unknown."},
	} {
		responses[item.code] = object{"description": item.description + " Errors may have no body.",
			"content": object{"application/json": object{"schema": object{"anyOf": []object{
				ref("GatewayError"), ref("k8s.io/apimachinery/pkg/apis/meta/v1.Status"),
			}}}}}
	}
	return responses
}

func watchResponse(op object) {
	response := op["responses"].(object)["200"].(object)
	response["description"] = "JobList, or newline-delimited WatchEvent objects when watch=true (not a JSON array)."
	response["content"] = object{"application/json": object{"schema": object{"anyOf": []object{resourceRef("JobList"), ref("WatchEvent")}}}}
}

func body(schema object, required bool) object {
	return object{"required": required, "content": object{"application/json": object{"schema": schema}}}
}

func setResourceRequest(op object, method string, schema object, abort bool) {
	switch method {
	case "post", "put":
		if !abort {
			op["requestBody"] = body(schema, true)
			op["description"] = op["description"].(string) + " On creation, provide metadata.name and resource input fields; UID, generation and timestamps are server-managed. For PUT, start from the complete GET object and preserve server metadata, resourceVersion, apiVersion and kind. Ordinary updates do not change stored status; /status updates must preserve spec and metadata."
		}
	case "patch":
		op["requestBody"] = object{"required": true, "description": "Merge patch or JSON Patch. Include resourceVersion (or a JSON Patch test) for concurrent-write protection; ordinary updates cannot modify status.",
			"content": object{
				"application/merge-patch+json": object{"schema": object{"type": "object", "additionalProperties": true}, "example": object{"metadata": object{"resourceVersion": "<current-version>"}}},
				"application/json-patch+json":  object{"schema": object{"type": "array", "items": record(object{"op": textSchema(), "path": textSchema(), "value": object{}, "from": textSchema()}, "op", "path")}},
			}}
	case "delete":
		op["requestBody"] = body(ref("k8s.io/apimachinery/pkg/apis/meta/v1.DeleteOptions"), false)
		op["description"] = op["description"].(string) + " Use DeleteOptions.preconditions.uid and resourceVersion from the current object. Only single-object deletion is exposed."
		op["responses"].(object)["200"] = object{"description": "Deleted object or Kubernetes Status.", "content": object{"application/json": object{"schema": object{"anyOf": []object{schema, ref("k8s.io/apimachinery/pkg/apis/meta/v1.Status")}}}}}
	case "head":
		for code := range op["responses"].(object) {
			op["responses"].(object)[code] = object{"description": "HTTP status only; HEAD has no response body."}
		}
	}
}

func permissions(resource, part, method string) ([]object, string) {
	read := method == "get" || method == "head"
	if resource == "configs" {
		if read && part == "object" {
			return optionalBearer, "Public Config may be read anonymously; OpsOnly requires Ops/Admin. Visibility is evaluated on the returned object."
		}
		return bearer, "Ops/Admin only. Built-in configs build-target, build-resource and services cannot be deleted."
	}
	if resource == "scripts" {
		if read {
			return bearer, "Authenticated users may read/list Scripts; Runner may GET only a named Script."
		}
		return bearer, "Ops/Admin only. Use UID and resourceVersion preconditions when deleting."
	}
	if resource == "runners" {
		return bearer, "Ops/Admin manage Runners; Runner tokens can register/update their own Runner and heartbeat via /status. Runner cannot delete or list all Runners. Deletion requires Offline or Evicted state. Eviction and cancellation use ordinary Runner updates."
	}
	if read {
		description := "Anonymous and authenticated users may read."
		if part == "list" {
			description += " Public lists are limited to at most 100 items per page; continue until metadata.continue is empty."
		}
		if resource == "jobs" && part == "object" {
			description += " Runner tokens may GET/HEAD only their assigned Jobs."
		}
		if resource == "jobs" && part == "list" && method == "get" {
			description += " Watch requires authenticated project access, returns newline-delimited WatchEvent JSON instead of JobList and cannot use field projection; restart List after 410."
		}
		return optionalBearer, description
	}
	if resource == "projects" {
		if method == "post" {
			return bearer, "Authenticated user creation; Gateway sets the owner label to the caller. Only Ops/Admin may select community Project type."
		}
		return bearer, "Project owner with user or Ops scope only; Admin has no override. Owner label is immutable."
	}
	if resource == "jobs" && part == "abort" {
		return bearer, "Project owner/member only, including Ops/Admin. Completed Jobs are returned unchanged. UID is required; optional reason is recorded."
	}
	if resource == "jobs" && part == "status" {
		return bearer, "Only the Runner assigned to this Job may update status. Gateway verifies assignment and protected fields."
	}
	if method == "delete" {
		return bearer, "Project owner or Admin; members cannot delete child resources."
	}
	return bearer, "Project owner/member or Admin. Ops has ordinary project-member access, not a cross-project override."
}

func queryParameters(list, watch, fields bool) []object {
	parameters := []object{}
	query := func(name, typ, description string) {
		parameters = append(parameters, object{"name": name, "in": "query", "description": description, "schema": object{"type": typ}})
	}
	query("includeFields", "string", "Comma-separated JSON paths, e.g. metadata,status.phase. GET/List only, not watch. Type metadata is preserved.")
	query("excludeFields", "string", "Comma-separated paths excluded after includeFields, e.g. spec. Missing paths are ignored.")
	if list {
		query("limit", "integer", "Page size. Public resource lists cap at 100; Runner assignment lists accept 1–500.")
		query("continue", "string", "Opaque token from metadata.continue. Keep the other query parameters unchanged; empty means the last page.")
		query("labelSelector", "string", "Kubernetes label selector; supports equality, inequality, in, notin and existence.")
		if fields {
			query("fieldSelector", "string", "Supported indexed fields depend on the resource; =, ==, != (not in). Example: status.phase=Running for Jobs.")
		}
	}
	if watch {
		query("watch", "boolean", "Authenticated watch only. Response is a stream of WatchEvent JSON, not a list; no includeFields/excludeFields.")
		query("resourceVersion", "string", "Resume from a List or watch resourceVersion.")
		query("resourceVersionMatch", "string", "Exact or NotOlderThan.")
		query("allowWatchBookmarks", "boolean", "Allow BOOKMARK watch events.")
		query("timeoutSeconds", "integer", "Watch duration in seconds; Runner tokens accept 1–300.")
	}
	return parameters
}

func describeFieldSelector(op object, kind string) {
	parameters, _ := op["parameters"].([]object)
	for _, parameter := range parameters {
		if parameter["name"] != "fieldSelector" {
			continue
		}
		scheme := runtime.NewScheme()
		if err := ebsv1.AddToScheme(scheme); err != nil {
			fail(err)
		}
		fields := []string{}
		for _, field := range []string{"metadata.name", "metadata.namespace", "status.phase", "status.stage", "status.runner", "status.release.phase"} {
			if _, _, err := scheme.ConvertFieldLabel(ebsv1.SchemeGroupVersion.WithKind(kind), field, ""); err == nil {
				fields = append(fields, field)
			}
		}
		parameter["description"] = "Comma-separated =, == or != comparisons (not in). Supported fields: " + strings.Join(fields, ", ") + "."
	}
}

func add(paths object, path, method string, op object) {
	parameters, _ := op["parameters"].([]object)
	for _, segment := range strings.Split(path, "/") {
		if strings.HasPrefix(segment, "{") {
			parameters = append(parameters, object{"name": strings.Trim(segment, "{}"), "in": "path", "required": true, "schema": textSchema()})
		}
	}
	if len(parameters) > 0 {
		op["parameters"] = parameters
	}
	if paths[path] == nil {
		paths[path] = object{}
	}
	paths[path].(object)[method] = op
}

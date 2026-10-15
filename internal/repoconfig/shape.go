package repoconfig

import (
	"fmt"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// requestedService is one entry of the services mapping as written, with its
// location for error messages. Version is the scalar's own text, so 17,
// "17" and 7.4 all read as written.
type requestedService struct {
	Kind, Version string
	Line, Column  int
}

// shape is what the AST check extracts before decoding.
type shape struct {
	services []requestedService
}

// Check AST types before decoding: YAML libraries may coerce a string or float
// to an integer or a numeric scalar to a string. This public contract does not.
func validateShape(data []byte) (shape, error) {
	// Retain duplicate entries only in the AST so we can report a stable category
	// without exposing the parser's source-bearing error. Reject before decoding.
	file, err := parser.ParseBytes(data, 0, parser.AllowDuplicateMapKey())
	if err != nil {
		return shape{}, safeYAMLError(err)
	}
	if len(file.Docs) != 1 {
		return shape{}, validationIssue("$", "invalid_document", "Provide exactly one YAML mapping; multiple or empty documents are not supported.")
	}
	mapping, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return shape{}, validationIssue("$", "invalid_document", "Configuration must be a YAML mapping; use '{}' for defaults.")
	}
	var found shape
	var issues []Issue
	seen := make(map[string]bool)
	for _, entry := range mapping.Values {
		key, ok := entry.Key.(*ast.StringNode)
		if !ok {
			issues = append(issues, shapeIssue("$", "invalid_key", "Use plain configuration keys; YAML merges and complex keys are not supported.", entry.Key))
			continue
		}
		field := key.Value
		if seen[field] {
			issues = append(issues, shapeIssue("$", "duplicate_key", "Remove the duplicate YAML key.", entry.Key))
			continue
		}
		seen[field] = true
		valid, fieldIssues, known := fieldShape(field, entry.Value, &found)
		issues = append(issues, fieldIssues...)
		if !known {
			issues = append(issues, shapeIssue("$", "unknown_field", "Unknown key; supported keys are context, build_timeout, dockerfile, port, env, health_check_path, resource_tier, services, setup, processes and docs_path.", entry.Key))
			continue
		}
		if !valid {
			issues = append(issues, shapeIssue(field, "invalid_type", "Use the documented field type; null, tags, anchors and aliases are not supported.", entry.Value))
		}
	}
	if len(issues) > 0 {
		return shape{}, &ValidationError{Issues: issues}
	}
	return found, nil
}

// fieldShape checks one top-level key's value. valid is whether the value
// has the field's type; issues are problems inside it; known is false for a
// key the contract does not have. Requested services are collected in found.
func fieldShape(field string, value ast.Node, found *shape) (valid bool, issues []Issue, known bool) {
	switch field {
	case "context", "build_timeout", "dockerfile", "health_check_path", "resource_tier", "docs_path":
		_, valid = value.(*ast.StringNode)
		if field == "build_timeout" && valid {
			valid = value.(*ast.StringNode).Value != ""
		}
	case "port":
		_, valid = value.(*ast.IntegerNode)
	case "env":
		sequence, isSequence := value.(*ast.SequenceNode)
		valid = isSequence
		if isSequence {
			for i, item := range sequence.Values {
				if _, ok := item.(*ast.StringNode); !ok {
					issues = append(issues, shapeIssue(fmt.Sprintf("env[%d]", i), "invalid_type", "Environment entries must be NAME=value strings.", item))
				}
			}
		}
	case "setup":
		valid = true
		issues = commandShape("setup", value)
	case "services":
		services, isMapping := value.(*ast.MappingNode)
		valid = isMapping
		if isMapping {
			found.services, issues = servicesShape(services)
		}
	case "processes":
		sequence, isSequence := value.(*ast.SequenceNode)
		valid = isSequence
		if isSequence {
			for i, item := range sequence.Values {
				issues = append(issues, processShape(fmt.Sprintf("processes[%d]", i), item)...)
			}
		}
	default:
		return false, nil, false
	}
	return valid, issues, true
}

// servicesShape reads `kind: version` entries. The kind itself is checked
// later, against the platform's catalog.
func servicesShape(mapping *ast.MappingNode) ([]requestedService, []Issue) {
	var services []requestedService
	var issues []Issue
	seen := map[string]bool{}
	for _, entry := range mapping.Values {
		key, ok := entry.Key.(*ast.StringNode)
		if !ok {
			issues = append(issues, shapeIssue("services", "invalid_key", "Name each service with a plain key: postgres, redis or mysql.", entry.Key))
			continue
		}
		if seen[key.Value] {
			issues = append(issues, shapeIssue("services", "duplicate_key", "Each service may be listed only once.", entry.Key))
			continue
		}
		seen[key.Value] = true
		var version string
		switch value := entry.Value.(type) {
		case *ast.StringNode:
			version = value.Value
		case *ast.IntegerNode, *ast.FloatNode:
			version = value.GetToken().Value
		default:
			issues = append(issues, shapeIssue("services", "invalid_type", "Give each service a version, such as postgres: 17.", entry.Value))
			continue
		}
		service := requestedService{Kind: key.Value, Version: version}
		if token := entry.Key.GetToken(); token != nil && token.Position != nil {
			service.Line, service.Column = token.Position.Line, token.Position.Column
		}
		services = append(services, service)
	}
	return services, issues
}

// commandShape requires an argv list: a sequence of strings.
func commandShape(field string, node ast.Node) []Issue {
	sequence, ok := node.(*ast.SequenceNode)
	if !ok {
		return []Issue{shapeIssue(field, "invalid_type", "Write the command as a list of arguments, such as [\"npm\", \"run\", \"migrate\"].", node)}
	}
	var issues []Issue
	for i, value := range sequence.Values {
		if _, ok := value.(*ast.StringNode); !ok {
			issues = append(issues, shapeIssue(fmt.Sprintf("%s[%d]", field, i), "invalid_type", "Each command argument must be a string; quote numbers.", value))
		}
	}
	return issues
}

// processShape requires a mapping of name, command and an optional
// resource_tier.
func processShape(field string, node ast.Node) []Issue {
	mapping, ok := node.(*ast.MappingNode)
	if !ok {
		return []Issue{shapeIssue(field, "invalid_type", "Describe each process as a mapping with name and command.", node)}
	}
	var issues []Issue
	seen := map[string]bool{}
	for _, entry := range mapping.Values {
		key, ok := entry.Key.(*ast.StringNode)
		if !ok {
			issues = append(issues, shapeIssue(field, "invalid_key", "Use plain keys: name, command and resource_tier.", entry.Key))
			continue
		}
		if seen[key.Value] {
			issues = append(issues, shapeIssue(field, "duplicate_key", "Remove the duplicate YAML key.", entry.Key))
			continue
		}
		seen[key.Value] = true
		switch key.Value {
		case "name", "resource_tier":
			if _, ok := entry.Value.(*ast.StringNode); !ok {
				issues = append(issues, shapeIssue(field+"."+key.Value, "invalid_type", "Use a string.", entry.Value))
			}
		case "command":
			issues = append(issues, commandShape(field+".command", entry.Value)...)
		default:
			issues = append(issues, shapeIssue(field, "unknown_field", "Unknown key; a process has name, command and resource_tier.", entry.Key))
		}
	}
	for _, required := range []string{"name", "command"} {
		if !seen[required] {
			issues = append(issues, shapeIssue(field+"."+required, "missing_field", "Every process needs a name and a command.", node))
		}
	}
	return issues
}

func shapeIssue(field, code, message string, node ast.Node) Issue {
	issue := Issue{Field: field, Code: code, Message: message}
	if node != nil && node.GetToken() != nil && node.GetToken().Position != nil {
		issue.Line, issue.Column = node.GetToken().Position.Line, node.GetToken().Position.Column
	}
	return issue
}

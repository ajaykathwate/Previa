package repoconfig

import (
	"fmt"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// Check AST types before decoding: YAML libraries may coerce a string or float
// to an integer or a numeric scalar to a string. This public contract does not.
func validateShape(data []byte) error {
	// Retain duplicate entries only in the AST so we can report a stable category
	// without exposing the parser's source-bearing error. Reject before decoding.
	file, err := parser.ParseBytes(data, 0, parser.AllowDuplicateMapKey())
	if err != nil {
		return safeYAMLError(err)
	}
	if len(file.Docs) != 1 {
		return validationIssue("$", "invalid_document", "Provide exactly one YAML mapping; multiple or empty documents are not supported.")
	}
	mapping, ok := file.Docs[0].Body.(*ast.MappingNode)
	if !ok {
		return validationIssue("$", "invalid_document", "Configuration must be a YAML mapping; use '{}' for defaults.")
	}
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
		valid := false
		switch field {
		case "dockerfile", "health_check_path", "resource_tier":
			_, valid = entry.Value.(*ast.StringNode)
		case "port":
			_, valid = entry.Value.(*ast.IntegerNode)
		case "env":
			sequence, isSequence := entry.Value.(*ast.SequenceNode)
			valid = isSequence
			if isSequence {
				for i, value := range sequence.Values {
					if _, ok := value.(*ast.StringNode); !ok {
						issues = append(issues, shapeIssue(fmt.Sprintf("env[%d]", i), "invalid_type", "Environment entries must be NAME=value strings.", value))
					}
				}
			}
		default:
			issues = append(issues, shapeIssue("$", "unknown_field", "Unknown key; supported keys are dockerfile, port, env, health_check_path and resource_tier.", entry.Key))
			continue
		}
		if !valid {
			issues = append(issues, shapeIssue(field, "invalid_type", "Use the documented field type; null, tags, anchors and aliases are not supported.", entry.Value))
		}
	}
	if len(issues) > 0 {
		return &ValidationError{Issues: issues}
	}
	return nil
}

func shapeIssue(field, code, message string, node ast.Node) Issue {
	issue := Issue{Field: field, Code: code, Message: message}
	if node != nil && node.GetToken() != nil && node.GetToken().Position != nil {
		issue.Line, issue.Column = node.GetToken().Position.Line, node.GetToken().Position.Column
	}
	return issue
}

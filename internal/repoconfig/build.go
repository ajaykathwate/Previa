package repoconfig

import (
	"fmt"
	"path"
	"time"
)

// resolveBuildSettings keeps repository requests within the operator's deadline.
func resolveBuildSettings(contextPath, timeout string, ceiling time.Duration) (string, time.Duration, []Issue) {
	if ceiling <= 0 {
		ceiling = 12 * time.Minute
	}
	var issues []Issue
	if contextPath != "." && !validDockerfile(contextPath) {
		issues = append(issues, Issue{Field: "context", Code: "invalid_context", Message: "Context must be '.' or a relative directory inside the repository, without traversal or an absolute path."})
	}
	duration := ceiling
	if timeout != "" {
		requested, err := time.ParseDuration(timeout)
		if err != nil || requested <= 0 || requested > ceiling {
			issues = append(issues, Issue{Field: "build_timeout", Code: "invalid_build_timeout", Message: fmt.Sprintf("Build timeout must be a positive duration, such as 10m, within the operator's limit of %s.", ceiling)})
		} else {
			duration = requested
		}
	}
	return path.Clean(contextPath), duration, issues
}

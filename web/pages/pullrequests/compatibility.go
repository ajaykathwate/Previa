package pullrequests

import "strings"

// compatibilityDiagnosis explains concrete registry and platform errors from
// this attempt's output. Generic failures keep their category diagnosis.
func compatibilityDiagnosis(code string, output []string) (Diagnosis, bool) {
	text := strings.ToLower(strings.Join(output, "\n"))
	has := func(parts ...string) bool {
		for _, part := range parts {
			if strings.Contains(text, part) {
				return true
			}
		}
		return false
	}
	stage := "start"
	if code == "build_failed" {
		stage = "image"
	}
	if has("no match for platform", "no matching manifest", "exec format error",
		"operating system \"windows\"", "image operating system (windows)", "image operating system 'windows'", "windows is not supported",
		"could not select device driver", "nvidia-smi: not found", "found no nvidia driver", "no cuda gpus are available") {
		return Diagnosis{Title: "This image needs a different platform", Stage: stage,
			Summary: "The image or workload cannot run on this Linux worker's CPU and available hardware.",
			Steps:   []string{"Use a Linux base image for the worker's architecture (usually amd64), or a compatible multi-platform image.", "ARM-only images on amd64 workers, Windows containers and GPU workloads are outside Previa's supported scope.", stepPushBuild}}, true
	}
	if code == "build_failed" && (has("unauthorized", "authentication required", "pull access denied", "401 unauthorized", "403 forbidden", "e401", "secret not found") || (has("secret ") && has(": not found"))) {
		return Diagnosis{Title: "The build needs registry credentials", Stage: "image",
			Summary: "A package or image pull reported an authentication error, or a required build secret was missing.",
			Steps:   []string{"Add a test credential in the repository's Secrets tab and explicitly allow Builds.", "Read private package credentials with RUN --mount=type=secret,id=NAME; use REGISTRY_AUTH (host username token per line) for private base images.", "Forks and unverified heads never receive build secrets or registry logins. Use public dependencies for those previews.", stepPushBuild}}, true
	}
	return Diagnosis{}, false
}

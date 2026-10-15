package pullrequests

import "strings"

// Diagnosis explains a failed build in the owner's words: what went wrong,
// what to do next, and the technical details behind it. It is derived only
// from the stored failure category and the end of the attempt's own output.
type Diagnosis struct {
	Title   string   // what went wrong, in plain words
	Summary string   // one sentence of context
	Steps   []string // what to do next, in order
	Stage   string   // the progress stage it stopped at (see Stages); "" when unknown
	Code    string   // the stored failure category, shown with the details
	Reason  string   // the most specific line from the build output, when one exists
	Retry   bool     // retrying alone may fix it (a temporary problem)
}

// sourceFailedPrefix starts the line the runtime writes when downloading the
// commit or reading its configuration fails.
const sourceFailedPrefix = "Source preparation failed: "

// Diagnose turns a failure category and the end of the output into a
// diagnosis. Unknown categories still get a useful answer.
func Diagnose(code string, output []string) Diagnosis {
	reason := failureReason(output)
	d := categoryDiagnosis(code)
	if code == "source_failed" {
		d = sourceDiagnosis(reason)
	}
	if code == "build_failed" || code == "crashed" || code == "unhealthy" || code == "runtime_unavailable" {
		if refined, ok := compatibilityDiagnosis(code, output); ok {
			d = refined
		}
	}
	d.Code, d.Reason = code, reason
	return d
}

// failureReason picks the line that says most precisely what went wrong:
// the runtime's own source failure, else the last error-looking line.
func failureReason(output []string) string {
	for i := len(output) - 1; i >= 0; i-- {
		if rest, found := strings.CutPrefix(strings.TrimSpace(output[i]), sourceFailedPrefix); found {
			return rest
		}
	}
	for i := len(output) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(output[i]); line != "" && LooksLikeError(line) {
			return line
		}
	}
	return ""
}

const (
	stepRetry     = "Choose Retry build."
	stepPushBuild = "Push the fix to this pull request's branch, then build the new commit."
	stepReadLog   = "Open the build log to see the full output around the error."
)

// categoryDiagnosis covers every stored failure category except source
// preparation, which sourceDiagnosis refines from the reason.
func categoryDiagnosis(code string) Diagnosis {
	switch code {
	case "worker_failed":
		return Diagnosis{Title: "The build worker could not finish", Summary: "Retries have ended for this request. Any earlier published preview stays available.", Retry: true,
			Steps: []string{"Ask a workspace operator to check the worker log and database migrations for the underlying error.", "After that problem is resolved, choose Retry build."}}

	case "build_failed":
		return Diagnosis{Title: "Your Dockerfile build failed", Stage: "image",
			Summary: "The image build stopped on an error from a step in your Dockerfile.",
			Steps:   []string{"Find the first error in the build output (shown in red under Technical details).", "Fix the failing step in your Dockerfile or code.", "Private dependencies need an explicitly allowed build secret in the Secrets tab; private base images use REGISTRY_AUTH. Fork builds receive neither.", stepPushBuild}}
	case "timeout":
		return Diagnosis{Title: "The build ran out of time", Stage: "image", Retry: true,
			Summary: "It took longer than the server allows for one build and was stopped.",
			Steps:   []string{"Check the build log for the slowest step, such as a large dependency install.", "Cache dependencies and keep the build context small with .dockerignore or a context subfolder.", "Set build_timeout in .previewrc.yaml within the operator's BUILD_TIMEOUT limit; source preparation and startup also consume that overall deadline.", stepRetry}}
	case "image_too_large":
		return Diagnosis{Title: "Your image is too large", Stage: "image",
			Summary: "The image exceeded the server's size limit (4 GiB by default) and was removed.",
			Steps:   []string{"Use a multi-stage Dockerfile and copy only the files needed at runtime.", "Exclude caches and build output with .dockerignore.", stepPushBuild}}
	case "secrets_unavailable":
		return Diagnosis{Title: "Repository secrets couldn't be unlocked", Stage: "start",
			Summary: "The server couldn't decrypt this repository's secrets, so the preview was not started.",
			Steps:   []string{"Check that the worker uses the same secrets encryption keys as the server.", "Restart the worker.", stepRetry}}
	case "capacity":
		return Diagnosis{Title: "The server had no room for another preview", Stage: "start", Retry: true,
			Summary: "Every preview slot was in use at that moment.",
			Steps:   []string{"Tear down previews you no longer need, from their pull requests or Previews.", stepRetry}}
	case "unhealthy":
		return Diagnosis{Title: "Your app didn't become ready", Stage: "start",
			Summary: "The preview started, but your app never answered its readiness check in time.",
			Steps: []string{"Make sure the app listens on the port set in .previewrc.yaml (3000 by default) on all interfaces (0.0.0.0), not only localhost.",
				"For nginx, use port 8080 or nginxinc/nginx-unprivileged: previews run without root privileges or capabilities. Update the app's port and the config together.",
				"If you set health_check_path, make sure that path answers with a 2xx status.", "Read the app's output at the end of the build log.", stepPushBuild}}
	case "crashed":
		return Diagnosis{Title: "Your app stopped while starting", Stage: "start",
			Summary: "The app's process exited before the preview was ready.",
			Steps:   []string{"Read the app's output at the end of the build log for the error.", "Check that every environment variable and secret the app needs is set.", "If nginx fails during startup, use port 8080 or nginxinc/nginx-unprivileged, and set port: 8080 in .previewrc.yaml. Keep writable caches and PID files in /tmp.", stepPushBuild}}
	case "service_unavailable":
		return Diagnosis{Title: "A database or cache image couldn't be downloaded", Stage: "start", Retry: true,
			Summary: "The server couldn't pull the PostgreSQL, Redis or MySQL image this preview asked for.",
			Steps:   []string{"This is usually temporary.", stepRetry, "If it keeps happening, check that the server can reach its image registry."}}
	case "service_failed":
		return Diagnosis{Title: "Your database or cache didn't start", Stage: "start",
			Summary: "PostgreSQL, Redis or MySQL did not become ready, or stopped while the preview started.",
			Steps:   []string{"Check that the service versions in .previewrc.yaml are ones this server offers.", "Read the service's output in the build log.", stepRetry}}
	case "setup_failed":
		return Diagnosis{Title: "Your setup command failed", Stage: "start",
			Summary: "The setup command (migrations or seed data) failed or ran out of time.",
			Steps:   []string{"Read the setup command's output in the build log.", "Fix the migration or seed step.", stepPushBuild}}
	case "process_failed":
		return Diagnosis{Title: "A background process stopped", Stage: "start",
			Summary: "A process declared in .previewrc.yaml stopped before the preview was ready.",
			Steps:   []string{"Read that process's output in the build log.", "Fix it.", stepPushBuild}}
	case "preview_missing":
		return Diagnosis{Title: "The preview stopped unexpectedly", Stage: "live", Retry: true,
			Summary: "Its container disappeared, so the address no longer serves it.",
			Steps:   []string{"Choose Retry build or Rebuild preview to start a new one at the same address."}}
	case "cancelled":
		return Diagnosis{Title: "The build was cancelled",
			Summary: "It stopped before a preview was published.",
			Steps:   []string{"Build again whenever you're ready."}}
	case "inaccessible":
		return Diagnosis{Title: "Previa lost access to this repository",
			Summary: "The GitHub App installation no longer includes this repository.",
			Steps:   []string{"On the Repositories page, use Choose repositories on GitHub and select this repository again.", "Then build again."}}
	case "runtime_unavailable":
		return Diagnosis{Title: "The preview service was unavailable", Stage: "start", Retry: true,
			Summary: "The server couldn't start containers at that moment.",
			Steps:   []string{"This is usually temporary.", stepRetry, "If it keeps happening, check that Docker is running on the build server."}}
	case "external_operation_failed", "generation_check_failed", "invalid_operation", "missing", "not_owned", "route_not_converged", "router_unavailable", "route_store_unavailable", "route_config_unwritable":
		return Diagnosis{Title: "The preview couldn't be published", Stage: "publish", Retry: true,
			Summary: "It built and started, but Previa couldn't give it its HTTPS address.",
			Steps:   []string{"This is usually temporary.", stepRetry, "If it keeps happening, check that the router is running on the server."}}
	default:
		return Diagnosis{Title: "The build failed", Stage: "image",
			Summary: "Previa couldn't build this preview.",
			Steps:   []string{stepReadLog, "Fix the problem on the branch and push, or choose Retry build if it was temporary."}}
	}
}

// sourceDiagnosis refines "the commit could not be prepared" from the exact
// reason the runtime recorded (its own messages, so matching is stable).
func sourceDiagnosis(reason string) Diagnosis {
	has := func(parts ...string) bool {
		lower := strings.ToLower(reason)
		for _, part := range parts {
			if strings.Contains(lower, part) {
				return true
			}
		}
		return false
	}
	d := Diagnosis{Stage: "code"}
	switch {
	case has("git submodules"):
		d.Title = "Your build context needs Git submodules"
		d.Summary = "GitHub's archive does not include submodule contents, so Previa stopped before building."
		d.Steps = []string{"Vendor the needed files into Git, or choose a context subfolder that does not use submodules.", "Automatic submodule fetching is not available.", stepPushBuild}
	case has("git lfs pointer"):
		d.Title = "Your build context contains Git LFS pointers"
		d.Summary = "The archive contains pointer files instead of the real assets, so Previa stopped before building."
		d.Steps = []string{"Include the real files in Git, or choose a context that does not need them.", "Automatic LFS fetching is not available.", stepPushBuild}
	case has("denied access", "access was denied"):
		d.Title = "GitHub didn't let Previa download your code"
		d.Summary = "GitHub refused to send this commit's files, so nothing could be built."
		d.Steps = []string{"On GitHub, open your GitHub App's settings → Permissions & events and set Repository permissions → Contents to Read-only.",
			"Accept the updated permissions on the installation (Settings → Applications → Installed GitHub Apps → Configure).",
			"Make sure this repository is selected in that installation.", stepRetry}
	case has("add .previewrc.yaml", "add the config file"):
		d.Title = "This commit has no .previewrc.yaml"
		d.Summary = "Previa needs a .previewrc.yaml file at the repository root to know how to run your app."
		d.Steps = []string{"Add .previewrc.yaml to the repository root. It can be as short as {} when your app listens on port 3000.",
			"Or open Starter kits in the sidebar: it writes the file for your stack, already checked.", stepPushBuild}
	case has("authentication is unavailable", "app configuration", "client configuration"):
		d.Title = "Previa couldn't sign in to GitHub as your app"
		d.Summary = "The worker's GitHub App credentials were missing or not accepted."
		d.Steps = []string{"Check the GitHub App ID and private key configured on the worker.", "Restart the worker.", stepRetry}
	case has("rate limit"):
		d.Title = "GitHub asked Previa to slow down"
		d.Summary = "GitHub's API rate limit was reached while downloading the commit."
		d.Steps = []string{"Wait a few minutes for the limit to reset.", stepRetry}
		d.Retry = true
	case has("could not be reached", "download failed", "interrupted", "ended early", "right now", "timed out", "cancelled"):
		d.Title = "Previa couldn't reach GitHub"
		d.Summary = "Downloading the commit failed part-way. This is usually temporary."
		d.Steps = []string{stepRetry, "If it keeps happening, check that the server can reach github.com."}
		d.Retry = true
	case has("dockerfile"):
		d.Title = "Previa couldn't find a usable Dockerfile"
		d.Summary = "The Dockerfile named in .previewrc.yaml is missing or is not a regular file."
		d.Steps = []string{"Check the dockerfile path in .previewrc.yaml (./Dockerfile by default) points to a file inside the repository.",
			"No Dockerfile yet? Starter kits in the sidebar writes one for your stack.", stepPushBuild}
	case has(".previewrc", "configuration", "yaml", "port must", "choose small", "environment variable", "process", "relative file path", "absolute http path", "absolute path on the preview", "command arguments", "name the program", "reserved"):
		d.Title = "Your .previewrc.yaml has a problem"
		d.Summary = "Previa read the file but one of its settings isn't valid."
		d.Steps = []string{"Fix the setting named under Technical details.", stepPushBuild}
	case has("unavailable, or this installation", "different repository", "requested commit", "verify access"):
		d.Title = "This commit isn't available to Previa"
		d.Summary = "GitHub couldn't provide this exact commit to the installation."
		d.Steps = []string{"Check the commit still exists on GitHub; a force-push may have replaced it.", "Check this repository is still selected in the GitHub App installation.", "Push a new commit, or choose Retry build."}
	case has("archive location", "host that is not allowed", "could not provide the repository archive"):
		d.Title = "GitHub's download couldn't be used"
		d.Summary = "GitHub answered with a download Previa doesn't accept. This is usually temporary."
		d.Steps = []string{stepRetry, "If it keeps happening, check that the server talks to github.com directly, not through a proxy that rewrites downloads."}
		d.Retry = true
	case has("size limit", "exceed", "entries"):
		d.Title = "Your repository is too large to download"
		d.Summary = "This commit exceeded a source safety limit: 256 MiB downloaded, 1 GiB unpacked or 100,000 entries by default."
		d.Steps = []string{"Remove large files or build output committed to the repository.", "A context subfolder reduces what reaches BuildKit, but the full GitHub archive still has to fit these limits.", stepPushBuild}
	case has("archive"):
		d.Title = "Your repository has files Previa can't safely unpack"
		d.Summary = "The source archive contains links, devices or paths that point outside the repository."
		d.Steps = []string{"Remove the entry named under Technical details.", stepPushBuild}
	case has("worker could not", "workspace"):
		d.Title = "The build server couldn't prepare a workspace"
		d.Summary = "It couldn't write this commit's files to disk. This is usually temporary."
		d.Steps = []string{stepRetry, "If it keeps happening, check the worker's free disk space."}
		d.Retry = true
	default:
		d.Title = "Previa couldn't prepare your code"
		d.Summary = "Downloading the commit or reading its .previewrc.yaml failed."
		d.Steps = []string{"Read the reason under Technical details.", "Fix it on the branch and push, or choose Retry build if it was temporary."}
	}
	return d
}

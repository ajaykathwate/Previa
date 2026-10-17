package environments

// Safe, user-facing sentences for the states and failure categories that
// reports and the dashboard show alike. Raw errors and log lines never appear
// here; the build log holds the details.

// WaitingTitle and WaitingMessage describe a request waiting for capacity or
// disk space. Nothing is promised about when or in what order it starts.
func WaitingTitle(reason string) string {
	switch reason {
	case WaitDisk:
		return "Waiting for disk space"
	case WaitPaused:
		return "Builds paused"
	case WaitAccount:
		return "Waiting for your other build"
	case WaitQuota:
		return "Daily build minutes used"
	}
	return "Waiting for capacity"
}

func WaitingMessage(reason string) string {
	switch reason {
	case WaitDisk:
		return "The build host is short of disk space. Previa frees space and starts this build automatically once there is enough."
	case WaitPaused:
		return "The operator paused new builds on this server. This build starts automatically when builds resume."
	case WaitAccount:
		return "Your account already has a build running on this server. This build starts automatically when it finishes, so every account gets a turn."
	case WaitQuota:
		return "Your account used today's build minutes on this server. This build starts automatically after midnight UTC."
	}
	return "Builds or previews already use this host's capacity. This build starts automatically once some frees up."
}

// FailureMessage is the safe sentence for a failure category. Raw errors and
// log lines never reach GitHub; the log page holds the details.
func FailureMessage(code string) string {
	switch code {
	case "worker_failed":
		return "The build worker could not finish this request and its retries have ended. Check the worker log for the underlying error, then retry this build. Any previously published preview stays available."

	case "source_failed":
		return "The commit could not be prepared (download, or an invalid .previewrc.yaml)."
	case "build_failed":
		return "The image build failed."
	case "secrets_unavailable":
		return "Repository secrets could not be decrypted on the server."
	case "capacity":
		return "The server had no capacity for another preview."
	case "unhealthy":
		return "The app did not become ready."
	case "crashed":
		return "The app exited while starting."
	case "service_unavailable":
		return "A managed service's image could not be pulled on the server."
	case "service_failed":
		return "A managed service (PostgreSQL or Redis) did not become ready or stopped."
	case "setup_failed":
		return "The setup command (migrations or seed data) failed or ran out of time."
	case "seed_failed":
		return "The seed command failed or ran out of time on the empty database."
	case "process_failed":
		return "A process declared next to the app stopped before the preview was ready."
	case "preview_missing":
		return "The preview's container disappeared. Retry, rebuild or push a commit to start a new one."
	case "timeout":
		return "The build took longer than allowed."
	case "cancelled":
		return "The build was cancelled."
	case "inaccessible":
		return "GitHub access to this repository was removed."
	case "runtime_unavailable":
		return "The preview infrastructure was unavailable."
	case "private_preview_unconfigured":
		return "Private previews need a configured authentication endpoint on every routing worker. Ask the operator to set PREVIEW_ACCESS_URL, restart the workers, and retry the build."
	case "missing", "not_owned", "route_not_converged", "router_unavailable", "route_store_unavailable", "route_config_unwritable":
		return "The preview could not be published."
	default:
		return "The preview could not be built."
	}
}

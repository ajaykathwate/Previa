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
	}
	return "Waiting for capacity"
}

func WaitingMessage(reason string) string {
	switch reason {
	case WaitDisk:
		return "The build host is short of disk space. Previa frees space and starts this build automatically once there is enough."
	case WaitPaused:
		return "The operator paused new builds on this server. This build starts automatically when builds resume."
	}
	return "Builds or previews already use this host's capacity. This build starts automatically once some frees up."
}

// FailureMessage is the safe sentence for a failure category. Raw errors and
// log lines never reach GitHub; the log page holds the details.
func FailureMessage(code string) string {
	switch code {
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
	case "missing", "not_owned", "route_not_converged", "router_unavailable", "route_store_unavailable", "route_config_unwritable":
		return "The preview could not be published."
	default:
		return "The preview could not be built."
	}
}

package reporting

import (
	"strings"
	"testing"
)

func TestDisconnectedCommentUsesLivingCommentOnceAndStopsRetries(t *testing.T) {
	for _, mode := range []string{"success", "permission", "outage", "human"} {
		t.Run(mode, func(t *testing.T) {
			f := newReportFixture(t)
			f.intent(t, "succeeded", 1)
			f.environment(t, "running", f.headSHA, 1, "")
			f.want(t)
			f.sync(t)
			comment := f.comment(t)
			var op string
			mustScan(t, f.pool.QueryRow(t.Context(), `INSERT INTO lifecycle_operations(kind,target_id,target_public_id,workspace_id,actor_id) SELECT 'repository',r.id,r.public_id,i.workspace_id,i.owner_user_id FROM repos r JOIN installations i ON i.id=r.installation_id WHERE r.id=$1 RETURNING id::text`, f.repoID), &op)
			f.exec(t, `INSERT INTO disconnect_comments(pull_request_id,operation_id) VALUES($1,$2)`, f.prID, op)
			before := f.github.count("PATCH comment")
			switch mode {
			case "permission":
				f.github.failOnce("GET repository", 403)
			case "outage":
				f.github.failOnce("GET repository", 503, 503, 503, 503)
			case "human":
				f.github.mu.Lock()
				f.github.comments[comment.ID].App = false
				f.github.comments[comment.ID].User = "human"
				f.github.mu.Unlock()
			}
			d := NewDisconnector(f.pool, f.github.api(t))
			var done bool
			for range 4 {
				var err error
				done, err = d.Disconnected(t.Context(), f.repoID)
				if err != nil {
					t.Fatal(err)
				}
				f.exec(t, `UPDATE disconnect_comments SET available_at=clock_timestamp() WHERE pull_request_id=$1`, f.prID)
			}
			if !done {
				t.Fatal("comment projection never finished")
			}
			if mode == "success" {
				if !strings.Contains(f.comment(t).Body, "Disconnected") || f.github.count("PATCH comment") != before+1 {
					t.Fatal("final comment missing or repeated")
				}
			} else if f.github.count("PATCH comment") != before {
				t.Fatal("unsafe final comment update")
			}
			var attempts int
			mustScan(t, f.pool.QueryRow(t.Context(), `SELECT attempts FROM disconnect_comments WHERE pull_request_id=$1`, f.prID), &attempts)
			if mode == "outage" && attempts != 3 {
				t.Fatal("outage retry bound", attempts)
			}
		})
	}
}

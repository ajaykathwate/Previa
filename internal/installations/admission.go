package installations

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

var ErrApprovalRequired = errors.New("this workspace needs early-access approval before connecting repositories")

type AdmissionGate interface {
	ApprovedTx(context.Context, pgx.Tx, int64) (bool, error)
}

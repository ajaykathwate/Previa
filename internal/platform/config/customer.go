package config

import (
	"errors"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"unicode"
)

// CustomerConfig contains operator-owned contact and product retention policy.
// Operator identities are immutable GitHub IDs, independent of workspace roles.
type CustomerConfig struct {
	EarlyAccessEnabled              bool   `envconfig:"EARLY_ACCESS_ENABLED" default:"true"`
	InstallationReconcileMaxChanges int    `envconfig:"INSTALLATION_RECONCILE_MAX_CHANGES" default:"5"`
	SupportEmail                    string `envconfig:"SUPPORT_EMAIL"`
	SupportOperatorGitHubIDs        string `envconfig:"SUPPORT_OPERATOR_GITHUB_IDS"`
	OperatorName                    string `envconfig:"OPERATOR_NAME" default:"Previa operator"`
	SupportMessageMaxBytes          int    `envconfig:"SUPPORT_MESSAGE_MAX_BYTES" default:"16384"`
	SupportRequestsPerDay           int    `envconfig:"SUPPORT_REQUESTS_PER_DAY" default:"5"`
	SupportRetentionDays            int    `envconfig:"SUPPORT_RETENTION_DAYS" default:"90"`
	BackupRetentionDays             int    `envconfig:"BACKUP_RETENTION_DAYS" default:"30"`
	DeletionJournalDays             int    `envconfig:"DELETION_JOURNAL_RETENTION_DAYS" default:"90"`
	DeletionJournalMaxBytes         int    `envconfig:"DELETION_JOURNAL_MAX_BYTES" default:"1048576"`
}

func (c CustomerConfig) OperatorIDs() ([]int64, error) {
	var ids []int64
	if strings.TrimSpace(c.SupportOperatorGitHubIDs) == "" {
		return ids, nil
	}
	for _, value := range strings.Split(c.SupportOperatorGitHubIDs, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || id <= 0 || len(ids) >= 20 {
			return nil, errors.New("SUPPORT_OPERATOR_GITHUB_IDS must contain at most 20 positive numeric GitHub IDs")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// EarlyAccessWithoutOperator reports that early access holds every new
// workspace for approval while nobody is configured to approve it.
func (c CustomerConfig) EarlyAccessWithoutOperator() bool {
	return c.EarlyAccessEnabled && strings.TrimSpace(c.SupportOperatorGitHubIDs) == ""
}

func (c CustomerConfig) checkCustomer() error {
	if c.InstallationReconcileMaxChanges < 1 || c.InstallationReconcileMaxChanges > 100 {
		return errors.New("INSTALLATION_RECONCILE_MAX_CHANGES must be between 1 and 100")
	}
	if c.SupportEmail != "" {
		address, err := mail.ParseAddress(c.SupportEmail)
		if err != nil || address.Address != c.SupportEmail || strings.ContainsAny(c.SupportEmail, "\r\n") {
			return errors.New("SUPPORT_EMAIL must be one email address without a display name")
		}
	}
	if strings.TrimSpace(c.OperatorName) == "" || len(c.OperatorName) > 160 || strings.ContainsFunc(c.OperatorName, unicode.IsControl) {
		return errors.New("OPERATOR_NAME must contain 1 to 160 bytes without control characters")
	}
	if _, err := c.OperatorIDs(); err != nil {
		return err
	}
	for _, v := range []struct {
		name            string
		value, min, max int
	}{
		{"SUPPORT_MESSAGE_MAX_BYTES", c.SupportMessageMaxBytes, 1024, 65536},
		{"SUPPORT_REQUESTS_PER_DAY", c.SupportRequestsPerDay, 1, 100},
		{"SUPPORT_RETENTION_DAYS", c.SupportRetentionDays, 1, 365},
		{"BACKUP_RETENTION_DAYS", c.BackupRetentionDays, 1, 365},
		{"DELETION_JOURNAL_RETENTION_DAYS", c.DeletionJournalDays, 2, 730},
		{"DELETION_JOURNAL_MAX_BYTES", c.DeletionJournalMaxBytes, 65536, 16777216},
	} {
		if v.value < v.min || v.value > v.max {
			return fmt.Errorf("%s must be between %d and %d", v.name, v.min, v.max)
		}
	}
	if c.DeletionJournalDays <= c.BackupRetentionDays {
		return errors.New("DELETION_JOURNAL_RETENTION_DAYS must exceed BACKUP_RETENTION_DAYS")
	}
	return nil
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/spf13/cobra"

	"github.com/nunocgoncalves/iterabase-mono/forge/internal/central"
)

// decimalCredits is a non-negative credit amount as central's decimal string.
var decimalCredits = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// newCentralOperator builds the operator client from the command's flags.
// Tests replace it with a fake.
var newCentralOperator = func(cmd *cobra.Command) (central.Operator, error) {
	centralURL, _ := cmd.Flags().GetString("central-url")
	caPath, _ := cmd.Flags().GetString("ca-bundle")
	region, _ := cmd.Flags().GetString("region")
	if centralURL == "" {
		return nil, errors.New("--central-url is required (or set FORGE_CENTRAL_URL)")
	}
	if caPath == "" {
		return nil, errors.New("--ca-bundle is required (or set FORGE_CENTRAL_CA_BUNDLE)")
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read --ca-bundle: %w", err)
	}
	awsCfg, err := config.LoadDefaultConfig(cmd.Context(), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	return central.NewClient(centralURL, ca, &central.STSAuthorizer{Credentials: awsCfg.Credentials, Region: region})
}

func newCentralCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "central",
		Short: "Manage installations on the central Iterabase inference gateway",
		Long: `central manages an installation's registry entry, entitlements and mTLS
certificate on the central Iterabase inference gateway (HOR-661).

Requests authenticate with the ambient AWS credentials (the Forge role, or the
CI preview role) through a presigned sts:GetCallerIdentity. Central's TLS
certificate is verified against --ca-bundle only.`,
	}
	cmd.PersistentFlags().String("central-url", os.Getenv("FORGE_CENTRAL_URL"), "central gateway URL, e.g. https://central.iterabase.com (env FORGE_CENTRAL_URL)")
	cmd.PersistentFlags().String("ca-bundle", os.Getenv("FORGE_CENTRAL_CA_BUNDLE"), "PEM file with the central CA certificate (env FORGE_CENTRAL_CA_BUNDLE)")
	cmd.PersistentFlags().String("region", "eu-west-1", "AWS region of the STS endpoint used for operator authentication")

	cmd.AddCommand(
		newCentralRegisterCmd(),
		newCentralIssueCmd(),
		newCentralRevokeCmd(),
		newCentralPlanCmd(),
		newCentralExtendTrialCmd(),
		newCentralShowCmd(),
		newCentralExportCmd(),
	)
	return cmd
}

func installationFlag(cmd *cobra.Command) {
	cmd.Flags().String("installation", "", "installation id, e.g. acme or pr-123")
	_ = cmd.MarkFlagRequired("installation")
}

func installationID(cmd *cobra.Command) (string, error) {
	id, _ := cmd.Flags().GetString("installation")
	if !central.InstallationIDPattern.MatchString(id) {
		return "", fmt.Errorf("--installation %q must match %s", id, central.InstallationIDPattern)
	}
	return id, nil
}

func oneOf(flag, value string, allowed []string) error {
	if slices.Contains(allowed, value) {
		return nil
	}
	return fmt.Errorf("--%s must be one of %s, got %q", flag, strings.Join(allowed, ", "), value)
}

func printJSON(cmd *cobra.Command, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		_, err = cmd.OutOrStdout().Write(append(raw, '\n'))
		return err
	}
	buf.WriteByte('\n')
	_, err := cmd.OutOrStdout().Write(buf.Bytes())
	return err
}

func newCentralRegisterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register an installation (idempotent; reactivates a revoked identical entry)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := installationID(cmd)
			if err != nil {
				return err
			}
			deployment, _ := cmd.Flags().GetString("deployment")
			plan, _ := cmd.Flags().GetString("plan")
			currency, _ := cmd.Flags().GetString("currency")
			for _, c := range []struct {
				flag, value string
				allowed     []string
			}{{"deployment", deployment, central.Deployments}, {"plan", plan, central.Plans}, {"currency", currency, central.Currencies}} {
				if err := oneOf(c.flag, c.value, c.allowed); err != nil {
					return err
				}
			}
			req := central.RegisterRequest{ID: id, Deployment: deployment, Plan: plan, BillingCurrency: currency}
			if cmd.Flags().Changed("trial-days") {
				days, _ := cmd.Flags().GetInt("trial-days")
				if days <= 0 {
					return errors.New("--trial-days must be positive")
				}
				req.TrialDays = &days
			}
			if plan == "trial" && req.TrialDays == nil {
				return errors.New("--trial-days is required for --plan trial")
			}
			if cmd.Flags().Changed("allowance") {
				allowance, _ := cmd.Flags().GetString("allowance")
				if !decimalCredits.MatchString(allowance) {
					return fmt.Errorf("--allowance must be a non-negative decimal number of credits, got %q", allowance)
				}
				req.AllowanceCredits = &allowance
			}
			op, err := newCentralOperator(cmd)
			if err != nil {
				return err
			}
			res, err := op.Register(cmd.Context(), req)
			if err != nil {
				return err
			}
			if res.Created {
				fmt.Fprintf(cmd.ErrOrStderr(), "registered %s\n", id)
			} else {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s is already registered with these settings\n", id)
			}
			return printJSON(cmd, res.Entry)
		},
	}
	installationFlag(cmd)
	cmd.Flags().String("deployment", "", "hosted, self_hosted_managed or internal")
	cmd.Flags().String("plan", "", "trial, starter, growth, scale or none")
	cmd.Flags().String("currency", "EUR", "billing currency: EUR or USD")
	cmd.Flags().Int("trial-days", 0, "trial length in days (plan trial)")
	cmd.Flags().String("allowance", "0", "monthly credit allowance as a decimal (self_hosted_managed; 0 = usage-only)")
	_ = cmd.MarkFlagRequired("deployment")
	_ = cmd.MarkFlagRequired("plan")
	return cmd
}

func newCentralIssueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "issue",
		Short: "Generate an installation key locally and have central issue its certificate",
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := installationID(cmd)
			if err != nil {
				return err
			}
			out, _ := cmd.Flags().GetString("out")
			manifest, _ := cmd.Flags().GetBool("secret-manifest")
			namespace, _ := cmd.Flags().GetString("namespace")
			name, _ := cmd.Flags().GetString("secret-name")
			if (out == "") == !manifest {
				return errors.New("set exactly one of --out <dir> or --secret-manifest")
			}
			op, err := newCentralOperator(cmd)
			if err != nil {
				return err
			}
			return issueCertificate(cmd.Context(), cmd, op, id, out, manifest, namespace, name)
		},
	}
	installationFlag(cmd)
	cmd.Flags().String("out", "", "directory for tls.key, tls.crt and ca.crt (mode 0600)")
	cmd.Flags().Bool("secret-manifest", false, "print a kubernetes.io/tls Secret manifest instead of writing files")
	cmd.Flags().String("namespace", "iterabase-system", "Secret namespace (--secret-manifest)")
	cmd.Flags().String("secret-name", "iterabase-central-client", "Secret name (--secret-manifest)")
	return cmd
}

func issueCertificate(ctx context.Context, cmd *cobra.Command, op central.Operator, id, out string, manifest bool, namespace, name string) error {
	kc, err := central.NewKeyAndCSR(id)
	if err != nil {
		return err
	}
	cert, err := op.Issue(ctx, id, kc.CSRPEM)
	if err != nil {
		return err
	}
	if manifest {
		_, err := fmt.Fprint(cmd.OutOrStdout(), central.SecretManifest(namespace, name, kc.KeyPEM, cert))
		return err
	}
	if err := central.WriteCertificateFiles(out, kc.KeyPEM, cert); err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "issued certificate %s for %s (expires %s) into %s\n",
		cert.Serial, id, cert.NotAfter.Format("2006-01-02"), out)
	return nil
}

func newCentralRevokeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke an installation and all its certificates (idempotent)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := installationID(cmd)
			if err != nil {
				return err
			}
			reason, _ := cmd.Flags().GetString("reason")
			if strings.TrimSpace(reason) == "" {
				return errors.New("--reason is required")
			}
			op, err := newCentralOperator(cmd)
			if err != nil {
				return err
			}
			res, err := op.Revoke(cmd.Context(), id, reason)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "revoked %s\n", id)
			return printJSON(cmd, res)
		},
	}
	installationFlag(cmd)
	cmd.Flags().String("reason", "", "audited reason, e.g. 'preview closed'")
	return cmd
}

func newCentralPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Convert a trial or change a hosted installation's plan",
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := installationID(cmd)
			if err != nil {
				return err
			}
			plan, _ := cmd.Flags().GetString("plan")
			if err := oneOf("plan", plan, []string{"starter", "growth", "scale"}); err != nil {
				return err
			}
			op, err := newCentralOperator(cmd)
			if err != nil {
				return err
			}
			res, err := op.SetPlan(cmd.Context(), id, plan)
			if err != nil {
				return err
			}
			return printJSON(cmd, res)
		},
	}
	installationFlag(cmd)
	cmd.Flags().String("plan", "", "starter, growth or scale")
	_ = cmd.MarkFlagRequired("plan")
	return cmd
}

func newCentralExtendTrialCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "extend-trial",
		Short: "Extend a trial and add the pro-rated credits for the extra days",
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := installationID(cmd)
			if err != nil {
				return err
			}
			days, _ := cmd.Flags().GetInt("days")
			if days <= 0 {
				return errors.New("--days must be positive")
			}
			op, err := newCentralOperator(cmd)
			if err != nil {
				return err
			}
			res, err := op.ExtendTrial(cmd.Context(), id, days)
			if err != nil {
				return err
			}
			return printJSON(cmd, res)
		},
	}
	installationFlag(cmd)
	cmd.Flags().Int("days", 0, "days to add to the trial")
	_ = cmd.MarkFlagRequired("days")
	return cmd
}

func newCentralShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show an installation's registry entry, certificates and current period",
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := installationID(cmd)
			if err != nil {
				return err
			}
			op, err := newCentralOperator(cmd)
			if err != nil {
				return err
			}
			res, err := op.Show(cmd.Context(), id)
			if err != nil {
				return err
			}
			return printJSON(cmd, res)
		},
	}
	installationFlag(cmd)
	return cmd
}

func newCentralExportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Download the monthly payload-free invoicing export",
		RunE: func(cmd *cobra.Command, _ []string) error {
			period, _ := cmd.Flags().GetString("period")
			if !periodPattern(period) {
				return fmt.Errorf("--period must be YYYY-MM, got %q", period)
			}
			format, _ := cmd.Flags().GetString("format")
			if err := oneOf("format", format, []string{"csv", "json"}); err != nil {
				return err
			}
			out, _ := cmd.Flags().GetString("out")
			op, err := newCentralOperator(cmd)
			if err != nil {
				return err
			}
			data, err := op.Export(cmd.Context(), period, format)
			if err != nil {
				return err
			}
			if out == "" {
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			if err := os.WriteFile(out, data, 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote %s\n", out)
			return nil
		},
	}
	cmd.Flags().String("period", "", "billing month, YYYY-MM")
	cmd.Flags().String("format", "csv", "csv or json")
	cmd.Flags().String("out", "", "write to path (default: stdout)")
	_ = cmd.MarkFlagRequired("period")
	return cmd
}

func periodPattern(p string) bool {
	if len(p) != 7 || p[4] != '-' {
		return false
	}
	for i, r := range p {
		if i != 4 && (r < '0' || r > '9') {
			return false
		}
	}
	month := (p[5]-'0')*10 + (p[6] - '0')
	return month >= 1 && month <= 12
}

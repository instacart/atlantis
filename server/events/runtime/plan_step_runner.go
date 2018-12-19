package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hashicorp/go-version"
	"github.com/runatlantis/atlantis/server/events/models"
)

const defaultWorkspace = "default"

var (
	plusDiffRegex  = regexp.MustCompile(`(?m)^ {2}\+`)
	tildeDiffRegex = regexp.MustCompile(`(?m)^ {2}~`)
	minusDiffRegex = regexp.MustCompile(`(?m)^ {2}-`)
)

type PlanStepRunner struct {
	TerraformExecutor TerraformExec
	DefaultTFVersion  *version.Version
}

func (p *PlanStepRunner) Run(ctx models.ProjectCommandContext, extraArgs []string, path string) (string, error) {
	tfVersion := p.DefaultTFVersion
	if ctx.ProjectConfig != nil && ctx.ProjectConfig.TerraformVersion != nil {
		tfVersion = ctx.ProjectConfig.TerraformVersion
	}

	// We only need to switch workspaces in version 0.9.*. In older versions,
	// there is no such thing as a workspace so we don't need to do anything.
	if err := p.switchWorkspace(ctx, path, tfVersion); err != nil {
		return "", err
	}

	planCmd := p.buildPlanCmd(ctx, extraArgs, path)
	output, err := p.TerraformExecutor.RunCommandWithVersion(ctx.Log, filepath.Clean(path), planCmd, tfVersion, ctx.Workspace)
	if err != nil {
		return output, err
	}
	return p.fmtPlanOutput(output), nil
}

// switchWorkspace changes the terraform workspace if necessary and will create
// it if it doesn't exist. It handles differences between versions.
func (p *PlanStepRunner) switchWorkspace(ctx models.ProjectCommandContext, path string, tfVersion *version.Version) error {
	// In versions less than 0.9 there is no support for workspaces.
	noWorkspaceSupport := MustConstraint("<0.9").Check(tfVersion)
	// If the user tried to set a specific workspace in the comment but their
	// version of TF doesn't support workspaces then error out.
	if noWorkspaceSupport && ctx.Workspace != defaultWorkspace {
		return fmt.Errorf("terraform version %s does not support workspaces", tfVersion)
	}
	if noWorkspaceSupport {
		return nil
	}

	// In version 0.9.* the workspace command was called env.
	workspaceCmd := "workspace"
	runningZeroPointNine := MustConstraint(">=0.9,<0.10").Check(tfVersion)
	if runningZeroPointNine {
		workspaceCmd = "env"
	}

	// Use `workspace show` to find out what workspace we're in now. If we're
	// already in the right workspace then no need to switch. This will save us
	// about ten seconds. This command is only available in > 0.10.
	if !runningZeroPointNine {
		workspaceShowOutput, err := p.TerraformExecutor.RunCommandWithVersion(ctx.Log, path, []string{workspaceCmd, "show"}, tfVersion, ctx.Workspace)
		if err != nil {
			return err
		}
		// If `show` says we're already on this workspace then we're done.
		if strings.TrimSpace(workspaceShowOutput) == ctx.Workspace {
			return nil
		}
	}

	// Finally we'll have to select the workspace. We need to figure out if this
	// workspace exists so we can create it if it doesn't.
	// To do this we can either select and catch the error or use list and then
	// look for the workspace. Both commands take the same amount of time so
	// that's why we're running select here.
	_, err := p.TerraformExecutor.RunCommandWithVersion(ctx.Log, path, []string{workspaceCmd, "select", "-no-color", ctx.Workspace}, tfVersion, ctx.Workspace)
	if err != nil {
		// If terraform workspace select fails we run terraform workspace
		// new to create a new workspace automatically.
		_, err = p.TerraformExecutor.RunCommandWithVersion(ctx.Log, path, []string{workspaceCmd, "new", "-no-color", ctx.Workspace}, tfVersion, ctx.Workspace)
		return err
	}
	return nil
}

func (p *PlanStepRunner) buildPlanCmd(ctx models.ProjectCommandContext, extraArgs []string, path string) []string {
	tfVars := p.tfVars(ctx)
	planFile := filepath.Join(path, GetPlanFilename(ctx.Workspace, ctx.ProjectConfig))

	// Check if env/{workspace}.tfvars exist and include it. This is a use-case
	// from Hootsuite where Atlantis was first created so we're keeping this as
	// an homage and a favor so they don't need to refactor all their repos.
	// It's also a nice way to structure your repos to reduce duplication.
	var envFileArgs []string
	envFile := filepath.Join(path, "env", ctx.Workspace+".tfvars")
	if _, err := os.Stat(envFile); err == nil {
		envFileArgs = []string{"-var-file", envFile}
	}

	argList := [][]string{
		// NOTE: we need to quote the plan filename because Bitbucket Server can
		// have spaces in its repo owner names.
		{"plan", "-input=false", "-refresh", "-no-color", "-out", fmt.Sprintf("%q", planFile)},
		tfVars,
		extraArgs,
		ctx.CommentArgs,
		envFileArgs,
	}

	return p.flatten(argList)
}

// tfVars returns a list of "-var", "key=value" pairs that identify who and which
// repo this command is running for. This can be used for naming the
// session name in AWS which will identify in CloudTrail the source of
// Atlantis API calls.
func (p *PlanStepRunner) tfVars(ctx models.ProjectCommandContext) []string {
	// NOTE: not using maps and looping here because we need to keep the
	// ordering for testing purposes.
	// NOTE: quoting the values because in Bitbucket the owner can have
	// spaces, ex -var atlantis_repo_owner="bitbucket owner".
	return []string{
		"-var",
		fmt.Sprintf("%s=%q", "atlantis_user", ctx.User.Username),
		"-var",
		fmt.Sprintf("%s=%q", "atlantis_repo", ctx.BaseRepo.FullName),
		"-var",
		fmt.Sprintf("%s=%q", "atlantis_repo_name", ctx.BaseRepo.Name),
		"-var",
		fmt.Sprintf("%s=%q", "atlantis_repo_owner", ctx.BaseRepo.Owner),
		"-var",
		fmt.Sprintf("%s=%d", "atlantis_pull_num", ctx.Pull.Num),
	}
}

func (p *PlanStepRunner) flatten(slices [][]string) []string {
	var flattened []string
	for _, v := range slices {
		flattened = append(flattened, v...)
	}
	return flattened
}

// fmtPlanOutput uses regex's to remove any leading whitespace in front of the
// terraform output so that the diff syntax highlighting works. Example:
// "  - aws_security_group_rule.allow_all" =>
// "- aws_security_group_rule.allow_all"
// We do it for +, ~ and -.
// It also removes the "Refreshing..." preamble.
func (p *PlanStepRunner) fmtPlanOutput(output string) string {
	// Plan output contains a lot of "Refreshing..." lines followed by a
	// separator. We want to remove everything before that separator.
	refreshSeparator := "------------------------------------------------------------------------\n"
	sepIdx := strings.Index(output, refreshSeparator)
	if sepIdx > -1 {
		output = output[sepIdx+len(refreshSeparator):]
	}

	output = plusDiffRegex.ReplaceAllString(output, "+")
	output = tildeDiffRegex.ReplaceAllString(output, "~")
	return minusDiffRegex.ReplaceAllString(output, "-")
}

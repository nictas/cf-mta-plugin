package commands

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudfoundry-incubator/multiapps-cli-plugin/clients/baseclient"
	"github.com/cloudfoundry-incubator/multiapps-cli-plugin/clients/models"
	"github.com/cloudfoundry-incubator/multiapps-cli-plugin/log"
	"github.com/cloudfoundry-incubator/multiapps-cli-plugin/ui"
	"github.com/cloudfoundry-incubator/multiapps-cli-plugin/util"
	"github.com/cloudfoundry/cli/cf/terminal"
	"github.com/cloudfoundry/cli/plugin"
)

// KubernetesDeployCommand is a command for deploying an MTA archive
type KubernetesDeployCommand struct {
	BaseCommand
	commandFlagsDefiner     CommandFlagsDefiner
	processParametersSetter ProcessParametersSetter
	processTypeProvider     ProcessTypeProvider
}

// NewKubernetesDeployCommand creates a new deploy command.
func NewKubernetesDeployCommand() *KubernetesDeployCommand {
	return &KubernetesDeployCommand{BaseCommand{}, deployCommandFlagsDefiner(), deployProcessParametersSetter(), &kubernetesDeployCommandProcessTypeProvider{}}
}

// GetPluginCommand returns the plugin command details
func (c *KubernetesDeployCommand) GetPluginCommand() plugin.Command {
	return plugin.Command{
		Name:     "k8s-deploy",
		HelpText: "Deploy a new multi-target app or sync changes to an existing one",
		UsageDetails: plugin.Usage{
			Usage: `Deploy a multi-target app archive
   cf k8s-deploy MTA [-e EXT_DESCRIPTOR[,...]] [-t TIMEOUT] [--version-rule VERSION_RULE] [-u URL] [-f] [--no-start] [--use-namespaces] [--no-namespaces-for-services] [--delete-services] [--delete-service-keys] [--delete-service-brokers] [--keep-files] [--no-restart-subscribed-apps] [--do-not-fail-on-missing-permissions] [--abort-on-error]

   Perform action on an active deploy operation
   cf k8s-deploy -i OPERATION_ID -a ACTION [-u URL]`,
			Options: map[string]string{
				extDescriptorsOpt:                                  "Extension descriptors",
				deployServiceURLOpt:                                "Deploy service URL, by default 'deploy-service.<system-domain>'",
				timeoutOpt:                                         "Start timeout in seconds",
				versionRuleOpt:                                     "Version rule (HIGHER, SAME_HIGHER, ALL)",
				operationIDOpt:                                     "Active deploy operation id",
				actionOpt:                                          "Action to perform on active deploy operation (abort, retry, monitor)",
				forceOpt:                                           "Force deploy without confirmation for aborting conflicting processes",
				util.GetShortOption(noStartOpt):                    "Do not start apps",
				util.GetShortOption(useNamespacesOpt):              "Use namespaces in app and service names",
				util.GetShortOption(noNamespacesForServicesOpt):    "Do not use namespaces in service names",
				util.GetShortOption(deleteServicesOpt):             "Recreate changed services / delete discontinued services",
				util.GetShortOption(deleteServiceKeysOpt):          "Delete existing service keys and apply the new ones",
				util.GetShortOption(deleteServiceBrokersOpt):       "Delete discontinued service brokers",
				util.GetShortOption(keepFilesOpt):                  "Keep files used for deployment",
				util.GetShortOption(noRestartSubscribedAppsOpt):    "Do not restart subscribed apps, updated during the deployment",
				util.GetShortOption(noFailOnMissingPermissionsOpt): "Do not fail on missing permissions for admin operations",
				util.GetShortOption(abortOnErrorOpt):               "Auto-abort the process on any errors",
			},
		},
	}
}

// Execute executes the command
func (c *KubernetesDeployCommand) Execute(args []string) ExecutionStatus {
	log.Tracef("Executing command '"+c.name+"': args: '%v'\n", args)

	var host string

	// Parse command arguments and check for required options
	flags, err := c.CreateFlags(&host)
	if err != nil {
		ui.Failed(err.Error())
		return Failure
	}
	optionValues := c.commandFlagsDefiner(flags)
	shouldExecuteActionOnExistingProcess, err := ContainsSpecificOptions(flags, args, map[string]string{"i": "-i", "a": "-a"})
	if err != nil {
		ui.Failed(err.Error())
		return Failure
	}
	var positionalArgNames []string
	if !shouldExecuteActionOnExistingProcess {
		positionalArgNames = []string{"MTA"}
	}
	err = c.ParseFlags(args, positionalArgNames, flags, nil)
	if err != nil {
		c.Usage(err.Error())
		return Failure
	}

	extDescriptors := GetStringOpt(extDescriptorsOpt, optionValues)
	operationID := GetStringOpt(operationIDOpt, optionValues)
	action := GetStringOpt(actionOpt, optionValues)
	force := GetBoolOpt(forceOpt, optionValues)

	context, err := c.GetContext()
	if err != nil {
		ui.Failed(err.Error())
		return Failure
	}

	if operationID != "" || action != "" {
		return c.ExecuteAction(operationID, action, host)
	}

	mtaArchive := args[0]

	// Print initial message
	ui.Say("Deploying multi-target app archive %s in org %s / space %s as %s...",
		terminal.EntityNameColor(mtaArchive), terminal.EntityNameColor(context.Org),
		terminal.EntityNameColor(context.Space), terminal.EntityNameColor(context.Username))

	// Get the full path of the MTA archive
	mtaArchivePath, err := filepath.Abs(mtaArchive)
	if err != nil {
		ui.Failed("Could not get absolute path of file '%s'", mtaArchive)
		return Failure
	}

	// TODO: Check if the MTA archive is a directory or a file

	// Get the full paths of the extension descriptors
	var extDescriptorPaths []string
	if extDescriptors != "" {
		extDescriptorFiles := strings.Split(extDescriptors, ",")
		for _, extDescriptorFile := range extDescriptorFiles {
			extDescriptorPath, err := filepath.Abs(extDescriptorFile)
			if err != nil {
				ui.Failed("Could not get absolute path of file '%s'", extDescriptorFile)
				return Failure
			}
			extDescriptorPaths = append(extDescriptorPaths, extDescriptorPath)
		}
	}

	// Extract mta id from archive file
	mtaID, err := util.GetMtaIDFromArchive(mtaArchivePath)
	if os.IsNotExist(err) {
		ui.Failed("Could not find file %s", terminal.EntityNameColor(mtaArchivePath))
		return Failure
	} else if err != nil {
		ui.Failed("Could not get MTA id from deployment descriptor: %s", err)
		return Failure
	}

	// Check for an ongoing operation for this MTA ID and abort it
	wasAborted, err := c.CheckOngoingOperation(mtaID, host, force)
	if err != nil {
		ui.Failed("Could not get MTA operations: %s", baseclient.NewClientError(err))
		return Failure
	}
	if !wasAborted {
		return Failure
	}

	// Check SLMP metadata
	// TODO: ensure session
	sessionProvider, err := c.NewSessionProvider(host)
	if err != nil {
		ui.Failed("Could not retrieve x-csrf-token provider for the current session: %s", baseclient.NewClientError(err))
		return Failure
	}
	err = sessionProvider.GetSession()
	if err != nil {
		ui.Failed("Could not retrieve x-csrf-token for the current session: %s", baseclient.NewClientError(err))
		return Failure
	}
	mtaClient, err := c.NewMtaClient(host)
	if err != nil {
		ui.Failed("Could not get space guid:", baseclient.NewClientError(err))
		return Failure
	}

	// Upload the MTA archive file
	mtaArchiveUploader := NewFileUploader([]string{mtaArchivePath}, mtaClient, sessionProvider)
	uploadedMtaArchives, status := mtaArchiveUploader.UploadFiles()
	if status == Failure {
		return Failure
	}
	var uploadedArchivePartIds []string
	for _, uploadedMtaArchivePart := range uploadedMtaArchives {
		uploadedArchivePartIds = append(uploadedArchivePartIds, uploadedMtaArchivePart.ID)
	}

	// Upload the extension descriptor files
	var uploadedExtDescriptorIDs []string
	if len(extDescriptorPaths) != 0 {
		extDescriptorsUploader := NewFileUploader(extDescriptorPaths, mtaClient, sessionProvider)
		uploadedExtDescriptors, status := extDescriptorsUploader.UploadFiles()
		if status == Failure {
			return Failure
		}
		for _, uploadedExtDescriptor := range uploadedExtDescriptors {
			uploadedExtDescriptorIDs = append(uploadedExtDescriptorIDs, uploadedExtDescriptor.ID)
		}
	}
	ui.Say("Starting deployment process...")

	// Build the process instance
	processBuilder := util.NewProcessBuilder()
	processBuilder.ProcessType(c.processTypeProvider.GetProcessType())
	processBuilder.Parameter("appArchiveId", strings.Join(uploadedArchivePartIds, ","))
	processBuilder.Parameter("mtaExtDescriptorId", strings.Join(uploadedExtDescriptorIDs, ","))
	processBuilder.Parameter("targetPlatform", context.Org+" "+context.Space)
	c.processParametersSetter(optionValues, processBuilder)
	operation := processBuilder.Build()

	err = sessionProvider.GetSession()
	if err != nil {
		ui.Failed("Could not retrieve x-csrf-token for the current session: %s", baseclient.NewClientError(err))
		return Failure
	}

	// Create the new process
	responseHeader, err := mtaClient.StartMtaOperation(*operation)
	if err != nil {
		ui.Failed("Could not create operation: %s", baseclient.NewClientError(err))
		return Failure
	}
	ui.Ok()
	return NewExecutionMonitorFromLocationHeader(c.name, responseHeader.Location.String(), []*models.Message{}, mtaClient).Monitor()
}

type kubernetesDeployCommandProcessTypeProvider struct{}

func (d kubernetesDeployCommandProcessTypeProvider) GetProcessType() string {
	return "KUBERNETES_DEPLOY"
}

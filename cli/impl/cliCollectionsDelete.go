package impl

import (
	"ffsyncclient/cli"
	"ffsyncclient/consts"
	"ffsyncclient/fferr"
	"ffsyncclient/langext"
	"ffsyncclient/syncclient"
)

type CLIArgumentsCollectionsDelete struct {
	Collection string
}

func NewCLIArgumentsCollectionsDelete() *CLIArgumentsCollectionsDelete {
	return &CLIArgumentsCollectionsDelete{}
}

func (a *CLIArgumentsCollectionsDelete) Mode() cli.Mode {
	return cli.ModeCollectionsDelete
}

func (a *CLIArgumentsCollectionsDelete) PositionArgCount() (*int, *int) {
	return langext.Ptr(1), langext.Ptr(1)
}

func (a *CLIArgumentsCollectionsDelete) AvailableOutputFormats() []cli.OutputFormat {
	return []cli.OutputFormat{cli.OutputFormatText}
}

func (a *CLIArgumentsCollectionsDelete) ShortHelp() [][]string {
	return [][]string{
		{"ffsclient delete <collection>", "Delete all the records in a collection"},
	}
}

func (a *CLIArgumentsCollectionsDelete) FullHelp() []string {
	return []string{
		"$> ffsclient delete <collection>",
		"",
		"Delete all the records in a collection",
	}
}

func (a *CLIArgumentsCollectionsDelete) Init(positionalArgs []string, optionArgs []cli.ArgumentTuple) error {
	a.Collection = positionalArgs[0]

	for _, arg := range optionArgs {
		return fferr.DirectOutput.New("Unknown argument: " + arg.Key)
	}

	return nil
}

func (a *CLIArgumentsCollectionsDelete) Execute(ctx *cli.FFSContext) error {
	ctx.PrintVerbose("[Delete Collection]")
	ctx.PrintVerbose("")
	ctx.PrintVerboseKV("Collection", a.Collection)

	// ========================================================================

	cfp, err := ctx.AbsSessionFilePath()
	if err != nil {
		return err
	}

	if !langext.FileExists(cfp) {
		return fferr.NewDirectOutput(consts.ExitcodeNoLogin, "Sessionfile does not exist.\nUse `ffsclient login <email> <password>` first")
	}

	// ========================================================================

	client := syncclient.NewFxAClient(ctx.Opt.AuthServerURL)

	ctx.PrintVerbose("Load existing session from " + cfp)
	session, err := syncclient.LoadSession(ctx, cfp)
	if err != nil {
		return err
	}

	session, err = client.AutoRefreshSession(ctx, session)
	if err != nil {
		return err
	}

	// ========================================================================

	err = client.DeleteCollection(ctx, session, a.Collection)
	if err != nil {
		return err
	}

	// ========================================================================

	if langext.Coalesce(ctx.Opt.Format, cli.OutputFormatText) != cli.OutputFormatText {
		return fferr.NewDirectOutput(consts.ExitcodeUnsupportedOutputFormat, "Unsupported output-format: "+ctx.Opt.Format.String())
	}

	ctx.PrintPrimaryOutput("Collection " + a.Collection + " deleted")
	return nil
}

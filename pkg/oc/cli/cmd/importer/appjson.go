package importer

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	kapi "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/kubectl/cmd/templates"
	kcmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/genericclioptions"
	"k8s.io/kubernetes/pkg/kubectl/genericclioptions/printers"
	"k8s.io/kubernetes/pkg/kubectl/genericclioptions/resource"

	configcmd "github.com/openshift/origin/pkg/bulk"
	"github.com/openshift/origin/pkg/oc/generate/app"
	"github.com/openshift/origin/pkg/oc/generate/appjson"
	appcmd "github.com/openshift/origin/pkg/oc/generate/cmd"
	"github.com/openshift/origin/pkg/oc/util/ocscheme"
	templateinternalclient "github.com/openshift/origin/pkg/template/client/internalversion"
	templateclientinternal "github.com/openshift/origin/pkg/template/generated/internalclientset"
	templateclient "github.com/openshift/origin/pkg/template/generated/internalclientset/typed/template/internalversion"
)

const AppJSONV1GeneratorName = "app-json/v1"

var (
	appJSONLong = templates.LongDesc(`
		Import app.json files as OpenShift objects

		app.json defines the pattern of a simple, stateless web application that can be horizontally scaled.
		This command will transform a provided app.json object into its OpenShift equivalent.
		During transformation fields in the app.json syntax that are not relevant when running on top of
		a containerized platform will be ignored and a warning printed.

		The command will create objects unless you pass the -o yaml or --as-template flags to generate a
		configuration file for later use.

		Experimental: This command is under active development and may change without notice.`)

	appJSONExample = templates.Examples(`
		# Import a directory containing an app.json file
	  $ %[1]s app.json -f .

	  # Turn an app.json file into a template
	  $ %[1]s app.json -f ./app.json -o yaml --as-template`)
)

type AppJSONOptions struct {
	PrintFlags *genericclioptions.PrintFlags

	Printer printers.ResourcePrinter

	bulkAction configcmd.BulkAction

	BaseImage        string
	Generator        string
	AsTemplate       string
	OutputVersionStr string

	OutputVersions []schema.GroupVersion

	Namespace string
	Client    templateclient.TemplateInterface

	genericclioptions.IOStreams
	resource.FilenameOptions
}

func NewAppJSONOptions(streams genericclioptions.IOStreams) *AppJSONOptions {
	return &AppJSONOptions{
		bulkAction: configcmd.BulkAction{
			IOStreams: streams,
		},
		IOStreams:  streams,
		PrintFlags: genericclioptions.NewPrintFlags("created").WithTypeSetter(ocscheme.PrintingInternalScheme),
		Generator:  AppJSONV1GeneratorName,
	}
}

// NewCmdAppJSON imports an app.json file (schema described here: https://devcenter.heroku.com/articles/app-json-schema)
// as a template.
func NewCmdAppJSON(fullName string, f kcmdutil.Factory, streams genericclioptions.IOStreams) *cobra.Command {
	o := NewAppJSONOptions(streams)
	cmd := &cobra.Command{
		Use:     "app.json -f APPJSON",
		Short:   "Import an app.json definition into OpenShift (experimental)",
		Long:    appJSONLong,
		Example: fmt.Sprintf(appJSONExample, fullName),
		Run: func(cmd *cobra.Command, args []string) {
			kcmdutil.CheckErr(o.Complete(f, cmd, args))
			kcmdutil.CheckErr(o.Validate())
			kcmdutil.CheckErr(o.Run())
		},
	}
	usage := "Filename, directory, or URL to app.json file to use"
	kcmdutil.AddJsonFilenameFlag(cmd.Flags(), &o.Filenames, usage)
	cmd.MarkFlagRequired("filename")
	cmd.Flags().StringVar(&o.BaseImage, "image", o.BaseImage, "An optional image to use as your base Docker build (must have ONBUILD directives)")
	cmd.Flags().StringVar(&o.Generator, "generator", o.Generator, "The name of the generator strategy to use - specify this value to for backwards compatibility.")
	cmd.Flags().StringVar(&o.AsTemplate, "as-template", o.AsTemplate, "If set, generate a template with the provided name")
	cmd.Flags().StringVar(&o.OutputVersionStr, "output-version", o.OutputVersionStr, "The preferred API versions of the output objects")
	o.bulkAction.BindForOutput(cmd.Flags(), "output", "template")
	o.PrintFlags.AddFlags(cmd)

	return cmd
}

func (o *AppJSONOptions) Complete(f kcmdutil.Factory, cmd *cobra.Command, args []string) error {
	for _, v := range strings.Split(o.OutputVersionStr, ",") {
		gv, err := schema.ParseGroupVersion(v)
		if err != nil {
			return fmt.Errorf("provided output-version %q is not valid: %v", v, err)
		}
		o.OutputVersions = append(o.OutputVersions, gv)
	}
	o.OutputVersions = append(o.OutputVersions, legacyscheme.Scheme.PrioritizedVersionsAllGroups()...)

	restMapper, err := f.ToRESTMapper()
	if err != nil {
		return err
	}
	clientConfig, err := f.ToRESTConfig()
	if err != nil {
		return err
	}
	dynamicClient, err := dynamic.NewForConfig(clientConfig)
	o.bulkAction.Bulk.Scheme = legacyscheme.Scheme
	o.bulkAction.Bulk.Op = configcmd.Creator{
		Client:     dynamicClient,
		RESTMapper: restMapper,
	}.Create

	o.Printer, err = o.PrintFlags.ToPrinter()
	if err != nil {
		return err
	}

	o.Namespace, _, err = f.ToRawKubeConfigLoader().Namespace()
	if err != nil {
		return err
	}

	templateClient, err := templateclientinternal.NewForConfig(clientConfig)
	if err != nil {
		return err
	}
	o.Client = templateClient.Template()

	return nil
}

func (o *AppJSONOptions) Validate() error {
	if len(o.Filenames) != 1 {
		return fmt.Errorf("you must provide the path to an app.json file or directory containing app.json")
	}
	switch o.Generator {
	case AppJSONV1GeneratorName:
	default:
		return fmt.Errorf("the generator %q is not supported, use: %s", o.Generator, AppJSONV1GeneratorName)
	}
	return nil
}

func (o *AppJSONOptions) Run() error {
	localPath, contents, err := contentsForPathOrURL(o.Filenames[0], o.In, "app.json")
	if err != nil {
		return err
	}

	g := &appjson.Generator{
		LocalPath: localPath,
		BaseImage: o.BaseImage,
	}
	switch {
	case len(o.AsTemplate) > 0:
		g.Name = o.AsTemplate
	case len(localPath) > 0:
		g.Name = filepath.Base(localPath)
	default:
		g.Name = path.Base(path.Dir(o.Filenames[0]))
	}
	if len(g.Name) == 0 {
		g.Name = "app"
	}

	template, err := g.Generate(contents)
	if err != nil {
		return err
	}

	template.ObjectLabels = map[string]string{"app.json": template.Name}

	// all the types generated into the template should be known
	if errs := app.AsVersionedObjects(template.Objects, legacyscheme.Scheme, legacyscheme.Scheme, o.OutputVersions...); len(errs) > 0 {
		for _, err := range errs {
			fmt.Fprintf(o.bulkAction.ErrOut, "error: %v\n", err)
		}
	}

	if o.bulkAction.ShouldPrint() || (o.bulkAction.Output == "name" && len(o.AsTemplate) > 0) {
		var obj runtime.Object
		if len(o.AsTemplate) > 0 {
			template.Name = o.AsTemplate
			obj = template
		} else {
			obj = &kapi.List{Items: template.Objects}
		}
		return o.Printer.PrintObj(obj, o.Out)
	}

	templateProcessor := templateinternalclient.NewTemplateProcessorClient(o.Client.RESTClient(), o.Namespace)
	result, err := appcmd.TransformTemplate(template, templateProcessor, o.Namespace, nil, false)
	if err != nil {
		return err
	}

	if o.bulkAction.Verbose() {
		appcmd.DescribeGeneratedTemplate(o.bulkAction.Out, "", result, o.Namespace)
	}

	if errs := o.bulkAction.WithMessage("Importing app.json", "creating").Run(&kapi.List{Items: result.Objects}, o.Namespace); len(errs) > 0 {
		return kcmdutil.ErrExit
	}
	return nil
}

func contentsForPathOrURL(s string, in io.Reader, subpaths ...string) (string, []byte, error) {
	switch {
	case s == "-":
		contents, err := ioutil.ReadAll(in)
		return "", contents, err
	case strings.Index(s, "http://") == 0 || strings.Index(s, "https://") == 0:
		_, err := url.Parse(s)
		if err != nil {
			return "", nil, fmt.Errorf("the URL passed to filename %q is not valid: %v", s, err)
		}
		res, err := http.Get(s)
		if err != nil {
			return "", nil, err
		}
		defer res.Body.Close()
		contents, err := ioutil.ReadAll(res.Body)
		return "", contents, err
	default:
		stat, err := os.Stat(s)
		if err != nil {
			return s, nil, err
		}
		if !stat.IsDir() {
			contents, err := ioutil.ReadFile(s)
			return s, contents, err
		}
		for _, sub := range subpaths {
			path := filepath.Join(s, sub)
			stat, err := os.Stat(path)
			if err != nil {
				continue
			}
			if stat.IsDir() {
				continue
			}
			contents, err := ioutil.ReadFile(s)
			return path, contents, err
		}
		return s, nil, os.ErrNotExist
	}
}

package main

// gkerun: Simple cluster and containerized app management on Google Cloud.
//
// Usage:
//     $ gkerun <cluster | app> <up | down> <project[/stack]> [flags]
//
// Common flags:
//     --location=x: set the Google Cloud location (default us-central1)
//
// For example:
//     # Spin up an entirely new cluster.
//     $ gkerun cluster up foobar/prod
//
//     # Spin up a new application from a custom-built image (published to private Google Cloud Registry).
//     $ gkerun app up foobar/prod --port=80
//
//     # Spin up a new application from a pre-built image.
//     $ gkerun app up foobar/prod --image="gcr.io/kuar-demo/kuard-amd64:blue" --port=80
//
//     # Scale an application's replica count.
//     $ gkerun app up foobar/prod --port=80 --replicas=5
//

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/joeduffy/gkerun/pkg/autoui"
	"github.com/pulumi/pulumi-docker/sdk/v3/go/docker"
	"github.com/pulumi/pulumi-gcp/sdk/v5/go/gcp/artifactregistry"
	"github.com/pulumi/pulumi-gcp/sdk/v5/go/gcp/container"
	k8s "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes"
	k8sappsv1 "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/apps/v1"
	k8scorev1 "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/core/v1"
	k8smetav1 "github.com/pulumi/pulumi-kubernetes/sdk/v3/go/kubernetes/meta/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"gopkg.in/alecthomas/kingpin.v2"
)

// Define our overall CLI structure.
var (
	app = kingpin.New("gkerun", "Simple cluster and containerized app management on Google Cloud.")

	appCmd     = app.Command("app", "Manage applications.")
	appUpCmd   = appCmd.Command("up", "Spin up or update an application.")
	_          = appUpCmd.Arg("env", "The <org/project/stack> to deploy into.").Required().String()
	appDownCmd = appCmd.Command("down", "Destroy an entire application.")
	_          = appDownCmd.Arg("env", "The <org/project/stack> to deploy into.").Required().String()

	clusterCmd     = app.Command("cluster", "Manage clusters.")
	clusterUpCmd   = clusterCmd.Command("up", "Spin up or update a cluster.")
	_              = clusterUpCmd.Arg("env", "The <org/project/stack> to deploy into.").Required().String()
	clusterDownCmd = clusterCmd.Command("down", "Destroy an entire cluster.")
	_              = clusterDownCmd.Arg("env", "The <org/project/stack> to deploy into.").Required().String()
)

func main() {
	// Configure our deployment.
	dep := &deployment{
		Location: app.Flag("location", "Set the Google cloud location to deploy into.").Default("us-central1").String(),
		Cluster:  appUpCmd.Flag("cluster", "The <org/project/stack> to fetch cluster info from.").String(),
		Image:    appUpCmd.Flag("image", "Image to deploy (build local if empty).").String(),
		Port:     appUpCmd.Flag("port", "port container listens on").Default("80").Int(),
		Replicas: appUpCmd.Flag("replicas", "number of load balanced replicas").Default("1").Int(),
	}

	// Parse flags and arguments.
	kingpin.Version("0.0.1")
	cmd := kingpin.MustParse(app.Parse(os.Args[1:]))
	if len(os.Args) < 4 {
		app.Fatalf("error: missing required environment <project/stack> argument")
	}

	// Detect the project name (used for both the Google and Pulumi project name).
	env := strings.Split(os.Args[3], "/")
	if len(env) != 3 {
		app.Fatalf("error: expected environment argument to have two parts, <org/project/stack>, found %d", len(env))
	}
	dep.OrgName = env[0]
	dep.Project = env[1]
	dep.StackName = env[2]

	// Determine the command flavors.
	var program pulumi.RunFunc
	if cmd == appUpCmd.FullCommand() || cmd == appDownCmd.FullCommand() {
		program = dep.DeclareAppInfra
	} else {
		program = dep.DeclareClusterInfra
	}
	var destroy bool
	if cmd == appDownCmd.FullCommand() || cmd == clusterDownCmd.FullCommand() {
		destroy = true
	}

	// Initialize the deployment and ensure we're ready to go.
	ctx := context.Background()
	if err := dep.Init(ctx, program); err != nil {
		app.Fatalf("error: could not create deployment for %s/%s (%s): %v",
			dep.Project, dep.StackName, dep.Location, err)
	}

	// Now perform the action based on the command.
	fmt.Printf("%s (%s/%s)\n",
		color.HiCyanString(cmd), color.HiMagentaString(dep.Project), color.MagentaString((dep.StackName)))
	outs, err := dep.Run(ctx, destroy)
	if err != nil {
		app.Fatalf("error: %s failed: %v", cmd, err)
	}

	// Finally, output the resulting kubeconfig or URL as appropriate.
	if outs != nil {
		switch cmd {
		case "app up":
			if url, ok := outs["url"].Value.(string); ok {
				fmt.Printf("🌍 %s:\n%s\n", color.HiYellowString("URL"), url)
			}
		case "cluster up":
			if kubeconfig, ok := outs["kubeconfig"].Value.(string); ok {
				fmt.Printf("🚢 %s:\n%s\n", color.HiYellowString("Kubeconfig"), kubeconfig)
			}
		}
	}
}

// deployment describes a cluster or application deployment.
type deployment struct {
	// Google Cloud settings:
	Project  string
	Location *string

	// Pulumi settings:
	OrgName   string
	StackName string

	// Deployment settings:
	Image    *string
	Port     *int
	Replicas *int
	Cluster  *string

	stack auto.Stack
}

func (dep *deployment) Init(ctx context.Context, program pulumi.RunFunc) error {
	// Create or select our target project and stack, to prepare for deploying. This ensures the
	// deployment state is recorded for history, reliability, and recovery in the event of failure.
	stack, err := auto.UpsertStackInlineSource(ctx, dep.StackName, dep.Project, program)
	if err != nil {
		return err
	}
	dep.stack = stack

	// Ensure the plugins used by the deployment are ready.
	if err := dep.stack.Workspace().InstallPlugin(ctx, "docker", "v3.0.0"); err != nil {
		return err
	}
	if err := dep.stack.Workspace().InstallPlugin(ctx, "gcp", "v5.2.0"); err != nil {
		return err
	}
	if err := dep.stack.Workspace().InstallPlugin(ctx, "kubernetes", "v3.5.2"); err != nil {
		return err
	}

	// Configure common Google project and location configuration variables.
	if err := dep.stack.SetConfig(ctx, "gcp:project", auto.ConfigValue{Value: dep.Project}); err != nil {
		return err
	}
	if err := dep.stack.SetConfig(ctx, "gcp:region", auto.ConfigValue{Value: *dep.Location}); err != nil {
		return err
	}
	return nil
}

func (dep *deployment) Run(ctx context.Context, destroy bool) (auto.OutputMap, error) {
	return autoui.Run(dep.stack, destroy)
}

func (dep *deployment) DeclareAppInfra(ctx *pulumi.Context) error {
	// Fetch the cluster configuration to use.
	k8sp, err := dep.getClusterProvider(ctx)
	if err != nil {
		return err
	}

	// Just use the supplied image, if present, otherwise build the image.
	var imageName pulumi.StringOutput
	if dep.Image != nil && *dep.Image != "" {
		imageName = pulumi.String(*dep.Image).ToStringOutput()
	} else {
		// Provision a Google Cloud Artifact Registry repo, and enable all
		// authenticated users to pull from and push to it.
		repoName := fmt.Sprintf("%s-repo", dep.StackName)
		repo, err := artifactregistry.NewRepository(ctx, repoName, &artifactregistry.RepositoryArgs{
			Location:     pulumi.String(*dep.Location),
			RepositoryId: pulumi.String(repoName),
			Format:       pulumi.String("DOCKER"),
		})
		if err != nil {
			return err
		}
		repoIAM, err := artifactregistry.NewRepositoryIamMember(
			ctx, repoName+"-iam", &artifactregistry.RepositoryIamMemberArgs{
				Location:   pulumi.String(*dep.Location),
				Repository: repo.Name,
				Role:       pulumi.String("roles/editor"),
				Member:     pulumi.String("allAuthenticatedUsers"),
			},
		)
		if err != nil {
			return err
		}

		// Build and publish our app to the repo.
		repoUrl := pulumi.Sprintf("%s-docker.pkg.dev/%s/%s/my-image", *dep.Location, dep.Project, repo.Name)
		image, err := docker.NewImage(ctx, "my-image", &docker.ImageArgs{
			ImageName: pulumi.Sprintf("%s:%d", repoUrl, pulumi.Int(time.Now().Unix())),
		}, pulumi.DependsOn([]pulumi.Resource{repoIAM}))
		if err != nil {
			return err
		}

		imageName = image.ImageName
	}

	// Now spin up a Kubernetes Namespace, Deployment, and Service that use this image.
	// The scaling and port information passed at the CLI will be used.
	appNS, err := k8scorev1.NewNamespace(ctx, dep.StackName+"-ns", &k8scorev1.NamespaceArgs{
		Metadata: &k8smetav1.ObjectMetaArgs{Name: pulumi.String(dep.StackName + "-ns")},
	}, pulumi.Provider(k8sp))
	if err != nil {
		return err
	}

	appLabels := pulumi.StringMap{"app": pulumi.String(dep.StackName)}
	appDep, err := k8sappsv1.NewDeployment(ctx, dep.StackName+"-dep", &k8sappsv1.DeploymentArgs{
		Metadata: &k8smetav1.ObjectMetaArgs{
			Namespace: appNS.Metadata.Elem().Name(),
		},
		Spec: k8sappsv1.DeploymentSpecArgs{
			Selector: &k8smetav1.LabelSelectorArgs{
				MatchLabels: appLabels,
			},
			Replicas: pulumi.Int(*dep.Replicas),
			Template: &k8scorev1.PodTemplateSpecArgs{
				Metadata: &k8smetav1.ObjectMetaArgs{
					Labels: appLabels,
				},
				Spec: &k8scorev1.PodSpecArgs{
					Containers: k8scorev1.ContainerArray{
						k8scorev1.ContainerArgs{
							Name:  pulumi.String(dep.StackName),
							Image: imageName,
						}},
				},
			},
		},
	}, pulumi.Provider(k8sp))
	if err != nil {
		return err
	}

	appSvc, err := k8scorev1.NewService(ctx, dep.StackName+"-svc", &k8scorev1.ServiceArgs{
		Metadata: &k8smetav1.ObjectMetaArgs{
			Namespace: appNS.Metadata.Elem().Name(),
			Labels:    appLabels,
		},
		Spec: &k8scorev1.ServiceSpecArgs{
			Ports: k8scorev1.ServicePortArray{
				k8scorev1.ServicePortArgs{
					Port:       pulumi.Int(*dep.Port),
					TargetPort: pulumi.Int(*dep.Port),
				},
			},
			Selector: appLabels,
			Type:     pulumi.String("LoadBalancer"),
		},
	}, pulumi.Provider(k8sp), pulumi.DependsOn([]pulumi.Resource{appDep}))
	if err != nil {
		return err
	}

	// TODO: fancy load balancing (Istio, blue/green, etc).

	// Export the load balancer URL at which the service may be accessed.
	ctx.Export("url", appSvc.Status.ApplyT(func(status *k8scorev1.ServiceStatus) *string {
		ingress := status.LoadBalancer.Ingress[0]
		if ingress.Hostname != nil {
			return ingress.Hostname
		}
		return ingress.Ip
	}))
	return nil
}

func (dep *deployment) getClusterProvider(ctx *pulumi.Context) (*k8s.Provider, error) {
	var kubeconfig pulumi.StringOutput
	if kp := os.Getenv("KUBECONFIG"); kp != "" {
		// If KUBECONFIG is set, just use the existing cluster.
		kcb, err := ioutil.ReadFile(kp)
		if err != nil {
			return nil, err
		}
		kubeconfig = pulumi.String(string(kcb)).ToStringOutput()
	} else {
		// Otherwise, look up the cluster we will be using, and use a StackReference to read its config.
		var clusterEnv string
		if dep.Cluster != nil && *dep.Cluster != "" {
			clusterEnv = *dep.Cluster
		} else {
			clusterEnv = fmt.Sprintf("%s/%s/default", dep.OrgName, dep.Project)
		}
		clusterStack, err := pulumi.NewStackReference(ctx, clusterEnv, nil)
		if err != nil {
			return nil, err
		}
		kubeconfig = clusterStack.GetStringOutput(pulumi.String("kubeconfig"))
	}
	return k8s.NewProvider(ctx, "k8s", &k8s.ProviderArgs{Kubeconfig: kubeconfig})
}

func (dep *deployment) DeclareClusterInfra(ctx *pulumi.Context) error {
	// Otherwise, spin up a cluster with Autopilot, and manufacture a
	// KUBECONFIG that uses gcloud for authentication.
	cluster, err := container.NewCluster(ctx, "cluster", &container.ClusterArgs{
		Location:        pulumi.String(*dep.Location),
		EnableAutopilot: pulumi.Bool(true),
	})
	if err != nil {
		return err
	}

	// Manufacture a Kubeconfig that uses Google Cloud auth, and export it.
	ctx.Export("kubeconfig", dep.getKubeconfig(cluster))
	return nil
}

// kubecontextTemplate specifies a gcloud-friendly KUBECONTEXT template.
// It uses the gcloud authentication helper rather than the default authentication.
const kubeconfigTemplate = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: %[1]s
    server: https://%[2]s
  name: %[3]s
contexts:
- context:
    cluster: %[3]s
    user: %[3]s
  name: %[3]s
current-context: %[3]s
kind: Config
preferences: {}
users:
- name: %[3]s
  user:
    auth-provider:
      config:
        cmd-args: config config-helper --format=json
        cmd-path: gcloud
        expiry-key: '{.credential.token_expiry}'
        token-key: '{.credential.access_token}'
      name: gcp
`

func (dep *deployment) getKubeconfig(cluster *container.Cluster) pulumi.StringOutput {
	zone1 := cluster.NodeLocations.ApplyT(func(zones []string) string { return zones[0] })
	return pulumi.Sprintf(kubeconfigTemplate,
		cluster.MasterAuth.ClusterCaCertificate().Elem(),
		cluster.Endpoint,
		pulumi.Sprintf("%s_%s_%s", dep.Project, zone1, cluster.Name),
	)
}

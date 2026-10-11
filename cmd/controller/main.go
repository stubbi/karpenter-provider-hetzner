package main

import (
	"github.com/awslabs/operatorpkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	// Register karpenter core types into the default k8s scheme.
	_ "sigs.k8s.io/karpenter/pkg/apis/v1"

	"sigs.k8s.io/karpenter/pkg/cloudprovider/overlay"
	"sigs.k8s.io/karpenter/pkg/controllers"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/operator"

	// Register our HCloudNodeClass v1 types.
	_ "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"

	hetznercp "github.com/paperclipinc/karpenter-provider-hetzner/pkg/cloudprovider"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/controllers/nodeclass"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/controllers/orphan"
	hetznerop "github.com/paperclipinc/karpenter-provider-hetzner/pkg/operator"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/imagefamily"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instance"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instancetype"
)

func main() {
	ctx, op := operator.NewOperator()

	cfg, err := hetznerop.LoadConfig()
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to load config")
		return
	}

	// Create the Hetzner Cloud API client. Built from cfg so the API timeout is
	// applied to the HTTP client the SDK is constructed with; it cannot be set
	// afterwards.
	hcloudClient, err := hetznerop.NewHCloudClient(cfg)
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to create Hetzner Cloud client")
		return
	}

	// Fail fast on a token Hetzner rejects, instead of starting healthy and failing
	// every create. Transient failures only warn: see ValidateToken.
	tokenWarning, err := hetznerop.ValidateToken(ctx, &hcloudClient.Server)
	if err != nil {
		log.FromContext(ctx).Error(err, "Hetzner API token check failed")
		return
	}
	if tokenWarning != nil {
		log.FromContext(ctx).Error(tokenWarning, "Hetzner API token check inconclusive")
	}

	// Create the three providers.
	instanceProvider := instance.NewProviderWithPlacementGroups(&hcloudClient.Server, &hcloudClient.PlacementGroup, cfg.ClusterName, &hcloudClient.Action)
	typeProvider := instancetype.NewProvider(&hcloudClient.ServerType)
	imageProvider := imagefamily.NewProvider(&hcloudClient.Image)

	// Create the cloud provider.
	baseCloudProvider := hetznercp.NewCloudProvider(
		op.GetClient(),
		instanceProvider,
		typeProvider,
		imageProvider,
	)

	// Wrap with the overlay decorator (required by NewControllers).
	cloudProvider := overlay.Decorate(baseCloudProvider, op.GetClient(), op.InstanceTypeStore)

	// Create cluster state.
	clusterState := state.NewCluster(op.Clock, op.GetClient(), cloudProvider)

	// Our NodeClass status controller (network + image validation, Ready).
	nodeClassController := nodeclass.NewController(op.GetClient(), &hcloudClient.Network, &hcloudClient.Location, &hcloudClient.Firewall, &hcloudClient.SSHKey, imageProvider)

	providerControllers := []controller.Controller{nodeClassController}
	if cfg.OrphanGCEnabled {
		// Reclaims servers created for a NodeClaim that no longer exists and that
		// never became a Node (#61). Core has no owner for that direction.
		providerControllers = append(providerControllers,
			orphan.NewController(op.GetClient(), instanceProvider, cfg.OrphanGCGracePeriod, op.Clock.Now))
	}

	// Wire and start all controllers.
	op.WithControllers(ctx, append(
		controllers.NewControllers(
			ctx,
			op.Manager,
			op.Clock,
			op.GetClient(),
			op.EventRecorder,
			cloudProvider,
			baseCloudProvider,
			clusterState,
			op.InstanceTypeStore,
		),
		providerControllers...,
	)...).Start(ctx)
}

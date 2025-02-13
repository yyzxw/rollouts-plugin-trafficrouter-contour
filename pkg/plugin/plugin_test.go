package plugin

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/argoproj-labs/rollouts-plugin-trafficrouter-contour/pkg/mocks"
	"github.com/argoproj-labs/rollouts-plugin-trafficrouter-contour/pkg/utils"

	"github.com/argoproj/argo-rollouts/pkg/apis/rollouts/v1alpha1"
	rolloutsPlugin "github.com/argoproj/argo-rollouts/rollout/trafficrouting/plugin/rpc"
	goPlugin "github.com/hashicorp/go-plugin"
	contourv1 "github.com/projectcontour/contour/apis/projectcontour/v1"
	"golang.org/x/exp/slog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	fakeDynClient "k8s.io/client-go/dynamic/fake"
)

var testHandshake = goPlugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "ARGO_ROLLOUTS_RPC_PLUGIN",
	MagicCookieValue: "trafficrouter",
}

func TestRunSuccessfully(t *testing.T) {
	utils.InitLogger()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := runtime.NewScheme()
	b := runtime.SchemeBuilder{
		contourv1.AddToScheme,
	}

	_ = b.AddToScheme(s)
	dynClient := fakeDynClient.NewSimpleDynamicClient(s, &mocks.HTTPProxyObj)
	rpcPluginImp := &RpcPlugin{
		IsTest:        true,
		dynamicClient: dynClient,
	}

	// pluginMap is the map of plugins we can dispense.
	var pluginMap = map[string]goPlugin.Plugin{
		"RpcTrafficRouterPlugin": &rolloutsPlugin.RpcTrafficRouterPlugin{Impl: rpcPluginImp},
	}

	ch := make(chan *goPlugin.ReattachConfig, 1)
	closeCh := make(chan struct{})
	go goPlugin.Serve(&goPlugin.ServeConfig{
		HandshakeConfig: testHandshake,
		Plugins:         pluginMap,
		Test: &goPlugin.ServeTestConfig{
			Context:          ctx,
			ReattachConfigCh: ch,
			CloseCh:          closeCh,
		},
	})

	// We should get a config
	var config *goPlugin.ReattachConfig
	select {
	case config = <-ch:
	case <-time.After(2000 * time.Millisecond):
		t.Fatal("should've received reattach")
	}
	if config == nil {
		t.Fatal("config should not be nil")
	}

	// Connect!
	c := goPlugin.NewClient(&goPlugin.ClientConfig{
		Cmd:             nil,
		HandshakeConfig: testHandshake,
		Plugins:         pluginMap,
		Reattach:        config,
	})
	client, err := c.Client()
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	// Pinging should work
	if err := client.Ping(); err != nil {
		t.Fatalf("should not err: %s", err)
	}

	// Kill which should do nothing
	c.Kill()
	if err := client.Ping(); err != nil {
		t.Fatalf("should not err: %s", err)
	}

	// Request the plugin
	raw, err := client.Dispense("RpcTrafficRouterPlugin")
	if err != nil {
		t.Fail()
	}

	pluginInstance := raw.(*rolloutsPlugin.TrafficRouterPluginRPC)
	err = pluginInstance.InitPlugin()
	if err.Error() != "" {
		t.Fail()
	}
	t.Run("SetWeight", func(t *testing.T) {
		var desiredWeight int32 = 30

		rollout := newRollout(mocks.StableServiceName, mocks.CanaryServiceName, mocks.HTTPProxyName)

		re := pluginInstance.SetWeight(
			rollout,
			desiredWeight,
			[]v1alpha1.WeightDestination{})

		if re.HasError() {
			t.Fail()
		}

		svcs := rpcPluginImp.UpdatedMockHTTPProxy.Spec.Routes[0].Services

		if 100-desiredWeight != int32(svcs[0].Weight) {
			t.Fail()
		}
		if desiredWeight != int32(svcs[1].Weight) {
			t.Fail()
		}
	})

	// Canceling should cause an exit
	cancel()
	<-closeCh
}

func newRollout(stableSvc, canarySvc, httpProxyName string) *v1alpha1.Rollout {
	contourConfig := ContourTrafficRouting{
		HTTPProxy: httpProxyName,
		Namespace: "default",
	}
	encodedContourConfig, err := json.Marshal(contourConfig)
	if err != nil {
		slog.Error("marshal the contour's config is failed", slog.Any("err", err))
		os.Exit(1)
	}

	return &v1alpha1.Rollout{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rollout",
			Namespace: "default",
		},
		Spec: v1alpha1.RolloutSpec{
			Strategy: v1alpha1.RolloutStrategy{
				Canary: &v1alpha1.CanaryStrategy{
					StableService: stableSvc,
					CanaryService: canarySvc,
					TrafficRouting: &v1alpha1.RolloutTrafficRouting{
						Plugins: map[string]json.RawMessage{
							"argoproj-labs/contour": encodedContourConfig,
						},
					},
				},
			},
		},
	}
}

package cost_test

import (
	"fmt"
	"math"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/AbdurahmanAlmehdi/gitops-platform/gate/internal/cost"
	"github.com/AbdurahmanAlmehdi/gitops-platform/pricing"
)

// table uses round numbers so every expected figure below can be verified by hand. A test
// whose expected value was produced by running the code proves only that the code is
// consistent with itself.
func table() *pricing.Table {
	return &pricing.Table{
		Spec: pricing.Spec{
			Currency:      "USD",
			HoursPerMonth: 100,
			Rates: pricing.Rates{
				CPU:     pricing.Rate{Unit: "core-hour", Price: 0.10},
				Memory:  pricing.Rate{Unit: "gib-hour", Price: 0.01},
				Storage: pricing.Rate{Unit: "gib-hour", Price: 0.001},
			},
			MissingRequests: pricing.MissingRequests{CPU: "500m", Memory: "512Mi", Flag: true},
		},
	}
}

func decode(t *testing.T, raw string) cost.Object {
	t.Helper()
	var obj cost.Object
	if err := yaml.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return obj
}

func estimate(t *testing.T, raw string) cost.Estimate {
	t.Helper()
	est, err := cost.New(table(), 3).Estimate([]cost.Object{decode(t, raw)})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	return est
}

func assertMoney(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.0001 {
		t.Errorf("monthly cost = %.4f, want %.4f", got, want)
	}
}

// TestKnownUsageTimesKnownRate is the LLD §4.6 pricing check: 2 cores at $0.10/core-hour
// plus 4 GiB at $0.01/GiB-hour is $0.24/hour, and 100 hours per month is $24.00.
func TestKnownUsageTimesKnownRate(t *testing.T) {
	est := estimate(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: demo}
spec:
  replicas: 2
  template:
    spec:
      containers:
        - name: app
          resources:
            requests:
              cpu: "1"
              memory: 2Gi
`)
	// Per replica: 1 core + 2 GiB = 0.10 + 0.02 = $0.12/hour. Two replicas = $0.24/hour.
	assertMoney(t, est.MonthlyUSD, 24.00)
}

// TestReplicaCountScalesCost is the property the whole cost gate rests on: the demo case
// is a replica spike, and if scaling replicas did not scale cost, the gate would never
// catch it.
func TestReplicaCountScalesCost(t *testing.T) {
	const manifest = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: demo}
spec:
  replicas: %d
  template:
    spec:
      containers:
        - name: app
          resources:
            requests: {cpu: "1", memory: 1Gi}
`
	one := estimate(t, fmt.Sprintf(manifest, 1))
	eight := estimate(t, fmt.Sprintf(manifest, 8))

	if eight.MonthlyUSD != one.MonthlyUSD*8 {
		t.Errorf("8 replicas cost %.2f, want exactly 8× the single-replica cost %.2f",
			eight.MonthlyUSD, one.MonthlyUSD*8)
	}
}

// TestInitContainersUseMaxNotSum encodes the Kubernetes scheduling rule. Summing init
// containers would overstate cost for every pod that uses them, and the error would be
// invisible because the total would still look plausible.
func TestInitContainersUseMaxNotSum(t *testing.T) {
	est := estimate(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: demo}
spec:
  replicas: 1
  template:
    spec:
      initContainers:
        - name: migrate
          resources: {requests: {cpu: "2", memory: 1Gi}}
        - name: seed
          resources: {requests: {cpu: "1", memory: 1Gi}}
      containers:
        - name: app
          resources: {requests: {cpu: "1", memory: 1Gi}}
`)
	// Init containers run one at a time, so the pod needs max(2, 1) = 2 cores for the
	// init phase and 1 core for the app phase, the requirement is the larger, 2 cores.
	// Memory likewise: max(1, 1) init vs 1 app = 1 GiB.
	// 2 cores × $0.10 + 1 GiB × $0.01 = $0.21/hour × 100 = $21.00.
	assertMoney(t, est.MonthlyUSD, 21.00)
}

// TestNativeSidecarsAddToTheTotal is the other half of the same rule: an init container
// with restartPolicy Always runs for the pod's whole life and genuinely does add.
func TestNativeSidecarsAddToTheTotal(t *testing.T) {
	est := estimate(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: demo}
spec:
  replicas: 1
  template:
    spec:
      initContainers:
        - name: proxy
          restartPolicy: Always
          resources: {requests: {cpu: "1", memory: 1Gi}}
      containers:
        - name: app
          resources: {requests: {cpu: "1", memory: 1Gi}}
`)
	// 2 cores × $0.10 + 2 GiB × $0.01 = $0.22/hour × 100 = $22.00.
	assertMoney(t, est.MonthlyUSD, 22.00)
}

// TestMissingRequestsArePricedAndFlagged guards the incentive: if a container with no
// requests were priced at zero, declaring nothing would be the cheapest way through the
// cost gate, and declaring nothing is exactly what the policy gate forbids.
func TestMissingRequestsArePricedAndFlagged(t *testing.T) {
	est := estimate(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: demo}
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: app
`)
	if est.MonthlyUSD <= 0 {
		t.Fatal("a container with no resource requests was priced at zero, which would make " +
			"declaring nothing the cheapest way past the cost gate")
	}
	// 0.5 cores × $0.10 + 0.5 GiB × $0.01 = $0.055/hour × 100 = $5.50.
	assertMoney(t, est.MonthlyUSD, 5.50)

	if len(est.Workloads) != 1 || len(est.Workloads[0].Flags) == 0 {
		t.Error("assumed defaults were applied without flagging them; the report would present " +
			"a guess with the same authority as a derived figure")
	}
}

func TestStatefulSetStorageScalesWithReplicas(t *testing.T) {
	est := estimate(t, `
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db, namespace: demo}
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: db
          resources: {requests: {cpu: "1", memory: 1Gi}}
  volumeClaimTemplates:
    - spec:
        resources:
          requests:
            storage: 10Gi
`)
	// Per replica: 1 core ($0.10) + 1 GiB ($0.01) + 10 GiB storage ($0.01) = $0.12/hour.
	// Three replicas = $0.36/hour × 100 = $36.00.
	assertMoney(t, est.MonthlyUSD, 36.00)
}

// TestJobsAreNotPricedSilently: a Job's cost depends on how long it runs and how often it
// fires, neither of which appears in the manifest. Reporting a confident number would be
// a guess formatted identically to a derived figure.
func TestJobsAreNotPricedSilently(t *testing.T) {
	est := estimate(t, `
apiVersion: batch/v1
kind: Job
metadata: {name: migrate, namespace: demo}
spec:
  template:
    spec:
      containers:
        - name: migrate
          resources: {requests: {cpu: "4", memory: 8Gi}}
`)
	assertMoney(t, est.MonthlyUSD, 0)
	if len(est.Workloads) != 1 || len(est.Workloads[0].Flags) == 0 {
		t.Error("a Job was skipped without saying so; the reader would assume it had been priced")
	}
}

func TestCompareProducesDeltaAndPercent(t *testing.T) {
	base := estimate(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: demo}
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: app
          resources: {requests: {cpu: "1", memory: 0}}
`)
	head := estimate(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: app, namespace: demo}
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: app
          resources: {requests: {cpu: "1", memory: 0}}
`)

	d := cost.Compare(base, head, base.MonthlyUSD)
	assertMoney(t, d.BaselineMonthlyUSD, 10.00)
	assertMoney(t, d.ProjectedMonthlyUSD, 30.00)
	assertMoney(t, d.DeltaMonthlyUSD, 20.00)
	if !d.HasPercentBasis {
		t.Fatal("percent basis missing despite a non-zero baseline")
	}
	if math.Abs(d.PercentIncrease-200) > 0.001 {
		t.Errorf("percent increase = %.2f, want 200", d.PercentIncrease)
	}
	if len(d.Workloads) != 1 || d.Workloads[0].Change != "modified" {
		t.Errorf("expected one modified workload, got %+v", d.Workloads)
	}
}

// TestNewWorkloadHasNoPercentBasis: dividing by a zero baseline is undefined, and
// reporting "+100%" for something that did not exist before would read as precise while
// meaning nothing.
func TestNewWorkloadHasNoPercentBasis(t *testing.T) {
	head := estimate(t, `
apiVersion: apps/v1
kind: Deployment
metadata: {name: new, namespace: demo}
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: app
          resources: {requests: {cpu: "1", memory: 0}}
`)
	d := cost.Compare(cost.Estimate{}, head, 0)
	if d.HasPercentBasis {
		t.Error("a workload with no baseline reported a percentage increase")
	}
	if len(d.Workloads) != 1 || d.Workloads[0].Change != "added" {
		t.Errorf("expected one added workload, got %+v", d.Workloads)
	}
}


// --- autoscaling awareness -------------------------------------------------
//
// An autoscaler is the largest cost decision a repository contains: it authorises a
// ceiling. It also touches no `replicas:` field, so a gate that prices the Deployment
// literally reports the cost of the quietest possible moment and calls it the cost of
// the change.

const autoscaledWorker = `
apiVersion: apps/v1
kind: Deployment
metadata: {name: demo-worker, namespace: demo}
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: worker
          resources: {requests: {cpu: "1", memory: 0}}
`

func TestScaledObjectPricesAtTheCeiling(t *testing.T) {
	scaledObject := `
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: {name: demo-worker, namespace: demo}
spec:
  scaleTargetRef:
    name: demo-worker
  minReplicaCount: 1
  maxReplicaCount: 10
`
	est, err := cost.New(table(), 3).Estimate([]cost.Object{
		decode(t, autoscaledWorker), decode(t, scaledObject),
	})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}

	// 1 core x $0.10 x 100 hours = $10/month per replica. Ten replicas authorised.
	assertMoney(t, est.MonthlyUSD, 100.00)

	w := est.Workloads[0]
	if !w.Autoscaled || w.MinReplicas != 1 || w.MaxReplicas != 10 {
		t.Fatalf("autoscale bounds not recorded: %+v", w)
	}
	// The floor still has to be reported, or the figure reads as the running cost.
	assertMoney(t, w.FloorMonthlyUSD, 10.00)
	if len(w.Flags) == 0 {
		t.Error("priced at the ceiling without saying so; the reader would take $100 for the running cost")
	}
}

// TestOmittedMaxReplicaCountUsesKedaDefault is the dangerous case. Leaving the field out
// is a one-line manifest that authorises a hundredfold increase, and nothing in the diff
// looks like a cost change.
func TestOmittedMaxReplicaCountUsesKedaDefault(t *testing.T) {
	scaledObject := `
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: {name: demo-worker, namespace: demo}
spec:
  scaleTargetRef:
    name: demo-worker
`
	est, err := cost.New(table(), 3).Estimate([]cost.Object{
		decode(t, autoscaledWorker), decode(t, scaledObject),
	})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	// KEDA's default maximum is 100 replicas: $10/replica x 100 = $1000/month.
	assertMoney(t, est.MonthlyUSD, 1000.00)
	if est.Workloads[0].MaxReplicas != 100 {
		t.Errorf("max replicas = %d, want KEDA's default of 100", est.Workloads[0].MaxReplicas)
	}
}

func TestHorizontalPodAutoscalerPricesAtTheCeiling(t *testing.T) {
	hpa := `
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: demo-worker, namespace: demo}
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: demo-worker
  minReplicas: 2
  maxReplicas: 5
`
	est, err := cost.New(table(), 3).Estimate([]cost.Object{
		decode(t, autoscaledWorker), decode(t, hpa),
	})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	assertMoney(t, est.MonthlyUSD, 50.00)
	assertMoney(t, est.Workloads[0].FloorMonthlyUSD, 20.00)
}

// TestAutoscalerTargetingAnotherWorkloadIsIgnored guards against bounds bleeding across
// workloads, an autoscaler on the API must not reprice the worker.
func TestAutoscalerTargetingAnotherWorkloadIsIgnored(t *testing.T) {
	other := `
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata: {name: demo-api, namespace: demo}
spec:
  scaleTargetRef:
    name: demo-api
  maxReplicaCount: 50
`
	est, err := cost.New(table(), 3).Estimate([]cost.Object{
		decode(t, autoscaledWorker), decode(t, other),
	})
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	assertMoney(t, est.MonthlyUSD, 10.00)
	if est.Workloads[0].Autoscaled {
		t.Error("an autoscaler targeting a different workload was applied to this one")
	}
}

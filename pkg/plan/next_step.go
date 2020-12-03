package plan

import (
	"fmt"
	"github.com/mitchellh/hashstructure"
	v1 "github.com/mmlt/environment-operator/api/v1"
	"github.com/mmlt/environment-operator/pkg/source"
	"github.com/mmlt/environment-operator/pkg/step"
	"k8s.io/apimachinery/pkg/types"
	"path/filepath"
	"strconv"
)

type Sourcer interface {
	Workspace(nsn types.NamespacedName, name string) (source.Workspace, bool)
}

// NextStep decides what Step should be executed next.
// A nil Step is returned when there is no work to do (because prerequisites like sources are missing or the target is
// already up-to-date).
//
// Current state is stored as hashes of source code and parameters in the Environment kind status.
// When a step hash doesn't match the hash stored in status the step will be executed.
func (p *Planner) NextStep(nsn types.NamespacedName, src Sourcer, destroy bool, ispec v1.InfraSpec, cspec []v1.ClusterSpec, status v1.EnvironmentStatus) (step.Step, error) {
	if len(stepFilter(status, v1.StateError)) > 0 {
		// a step is in error state (it needs to be reset to continue)
		return nil, nil
	}

	running := stepFilter(status, v1.StateRunning)
	if len(running) > 0 {
		// a step is already running, return it.
		st, ok := p.currentPlanStep(nsn, running[0])
		if ok {
			return st, nil
		}
		// currentPlan maybe empty due to program restart.
	}

	// Replace references to secret values with the value from vault.
	ispec, err := vaultInfraValues(ispec, p.Cloud)
	if err != nil {
		return nil, fmt.Errorf("vault ref: %w", err)
	}

	ok := p.buildPlan(nsn, src, destroy, ispec, cspec)
	if !ok {
		return nil, nil
	}

	if len(running) > 0 {
		// a step is already running and current plan is just built.
		st, ok := p.currentPlanStep(nsn, running[0])
		if ok {
			return st, nil
		}
		p.Log.Info("NextStep unexpected step name %s in status.steps", running[0])
	}

	st, err := p.selectStep(nsn, status)

	if st != nil {
		p.Log.V(2).Info("NextStep", "request", nsn, "name", st.Meta().ID.ShortName())
	}

	return st, err
}

// BuildPlan builds a plan containing the steps to create/update/delete a target environment.
// An environment is identified by nsn.
// Returns false if not all prerequisites are fulfilled.
func (p *Planner) buildPlan(nsn types.NamespacedName, src Sourcer, destroy bool, ispec v1.InfraSpec, cspec []v1.ClusterSpec) bool {
	p.Lock()
	defer p.Unlock()

	if p.currentPlans == nil {
		p.currentPlans = make(map[types.NamespacedName]plan)
	}

	var pl plan
	var ok bool
	switch {
	case destroy:
		pl, ok = p.buildDestroyPlan(nsn, src, ispec, cspec)

	default:
		pl, ok = p.buildCreatePlan(nsn, src, ispec, cspec)
	}
	if !ok {
		return false
	}

	p.currentPlans[nsn] = planFilter(pl, p.AllowedStepTypes)

	return true
}

// BuildDestroyPlan builds a plan to delete a target environment.
// Returns false if workspaces are not prepped with sources.
func (p *Planner) buildDestroyPlan(nsn types.NamespacedName, src Sourcer, ispec v1.InfraSpec, cspec []v1.ClusterSpec) (plan, bool) {
	tfw, ok := src.Workspace(nsn, "")
	if !ok || tfw.Hash == "" {
		return nil, false
	}
	tfPath := filepath.Join(tfw.Path, ispec.Main)

	h := p.hash(tfw.Hash)

	pl := make(plan, 0, 1)
	pl = append(pl,
		&step.DestroyStep{
			Metaa: stepMeta(nsn, "", step.TypeDestroy, h),
			Values: step.InfraValues{
				Infra:    ispec,
				Clusters: cspec,
			},
			SourcePath: tfPath,
			Cloud:      p.Cloud,
			Terraform:  p.Terraform,
		})

	return pl, true
}

// BuildCreatePlan builds a plan to create or update a target environment.
func (p *Planner) buildCreatePlan(nsn types.NamespacedName, src Sourcer, ispec v1.InfraSpec, cspec []v1.ClusterSpec) (plan, bool) {
	tfw, ok := src.Workspace(nsn, "")
	if !ok || !tfw.Synced {
		return nil, false
	}
	tfPath := filepath.Join(tfw.Path, ispec.Main)

	var cspecInfra []interface{}
	for _, s := range cspec {
		cspecInfra = append(cspecInfra, s.Infra)
	}
	h := p.hash(tfw.Hash, ispec, cspecInfra)

	pl := make(plan, 0, 1+4*len(cspec))
	pl = append(pl,
		&step.InfraStep{
			Metaa: stepMeta(nsn, "", step.TypeInfra, h),
			Values: step.InfraValues{
				Infra:    ispec,
				Clusters: cspec,
			},
			SourcePath: tfPath,
			Cloud:      p.Cloud,
			Terraform:  p.Terraform,
		})

	for _, cl := range cspec {
		cw, ok := src.Workspace(nsn, cl.Name)
		if !ok || cw.Hash == "" {
			return nil, false
		}

		kcPath := filepath.Join(cw.Path, "kubeconfig")
		mvPath := filepath.Join(cw.Path, cl.Addons.MKV)

		az := p.Azure
		az.SetSubscription(ispec.AZ.Subscription)
		pl = append(pl,
			&step.AKSPoolStep{
				Metaa:         stepMeta(nsn, cl.Name, step.TypeAKSPool, p.hash(tfw.Hash, ispec.AZ.ResourceGroup, cl.Infra.Version)),
				ResourceGroup: ispec.AZ.ResourceGroup,
				Cluster:       prefixedClusterName("aks", ispec.EnvName, cl.Name),
				Version:       cl.Infra.Version,
				Azure:         az,
			},
			&step.KubeconfigStep{
				Metaa:       stepMeta(nsn, cl.Name, step.TypeKubeconfig, p.hash(tfw.Hash)),
				TFPath:      tfPath,
				ClusterName: cl.Name,
				KCPath:      kcPath,
				Access:      ispec.State.Access,
				Cloud:       p.Cloud,
				Terraform:   p.Terraform,
				Kubectl:     p.Kubectl,
			},
			&step.AKSAddonPreflightStep{
				Metaa:   stepMeta(nsn, cl.Name, step.TypeAKSAddonPreflight, h),
				KCPath:  kcPath,
				Kubectl: p.Kubectl,
			},
			&step.AddonStep{
				Metaa:           stepMeta(nsn, cl.Name, step.TypeAddons, p.hash(cw.Hash, cl.Addons.Jobs, cl.Addons.X)),
				SourcePath:      cw.Path,
				KCPath:          kcPath,
				MasterVaultPath: mvPath,
				JobPaths:        cl.Addons.Jobs,
				Values:          cl.Addons.X,
				Addon:           p.Addon,
			},
		)
	}

	return pl, true
}

func stepMeta(nsn types.NamespacedName, clusterName string, typ step.Type, hash string) step.Metaa {
	return step.Metaa{
		ID: step.ID{
			Type:        typ,
			Namespace:   nsn.Namespace,
			Name:        nsn.Name,
			ClusterName: clusterName,
		},
		Hash: hash,
	}
}

// SelectStep returns the next step to execute from current plan.
// NB. The returned Step might be in Running state (it's up to the executor to accept the step or not)
func (p *Planner) selectStep(nsn types.NamespacedName, status v1.EnvironmentStatus) (step.Step, error) {
	pl, ok := p.currentPlan(nsn)
	if !ok {
		return nil, fmt.Errorf("expected plan for: %v", nsn)
	}

	for _, st := range pl {
		id := st.Meta().ID

		// Get current step state.
		current, ok := status.Steps[id.ShortName()]
		if !ok {
			// first time this step is seen
			return st, nil
		}

		// Checking hash before state has the effect that steps that are in error state but not changed are skipped.
		// A step can get such a state when the following sequence of events take place;
		//	1. step source or parameter are changed
		//	2. step runs but errors
		//	3. changes from 1 are undone

		if current.Hash == st.Meta().Hash {
			continue
		}

		if current.State == v1.StateError {
			//TODO consider introducing error retry budgets to allow retry after error

			// no budget to retry
			return nil, nil
		}

		return st, nil
	}

	return nil, nil
}

// Hash returns a string that is unique for args.
// Errors are logged but not returned.
func (p *Planner) hash(args ...interface{}) string {
	i, err := hashstructure.Hash(args, nil)
	if err != nil {
		p.Log.Error(err, "hash")
		return "hasherror"
	}
	return strconv.FormatUint(i, 16)
}

// PrefixedClusterName returns the name as it's used in Azure.
// NB. the same algo is in terraform
func prefixedClusterName(resource, env, name string) string {
	t := env[len(env)-1:]
	return fmt.Sprintf("%s%s001%s-%s", t, resource, env, name)
}

// StepFilter returns the names of the steps that match state.
func stepFilter(status v1.EnvironmentStatus, state v1.StepState) []string {
	var r []string
	for n, s := range status.Steps {
		if s.State == state {
			r = append(r, n)
		}
	}
	return r
}

// PlanFilter returns plan with only the steps that are allowed.
// If allowed is nil plan is returned as-is.
func planFilter(pl plan, allowed map[step.Type]struct{}) plan {
	if len(allowed) == 0 {
		return pl
	}

	r := make(plan, 0, len(pl))
	for _, v := range pl {
		if _, ok := allowed[v.Meta().ID.Type]; ok {
			r = append(r, v)
		}
	}

	return r
}

package update

import (
	"context"
	"fmt"
	"github.com/fabric8-services/fabric8-common/log"
	"github.com/fabric8-services/fabric8-tenant/auth"
	"github.com/fabric8-services/fabric8-tenant/cluster"
	"github.com/fabric8-services/fabric8-tenant/configuration"
	"github.com/fabric8-services/fabric8-tenant/dbsupport"
	"github.com/fabric8-services/fabric8-tenant/environment"
	"github.com/fabric8-services/fabric8-tenant/sentry"
	"github.com/fabric8-services/fabric8-tenant/tenant"
	"github.com/fabric8-services/fabric8-tenant/utils"
	"github.com/jinzhu/gorm"
	"sync"
	"time"
)

type followUpFunc func() error

type Executor interface {
	Update(ctx context.Context, dbTenant *tenant.Tenant, user *auth.User, envTypes []environment.Type, allowSelfHealing bool) error
}

func NewTenantsUpdater(
	db *gorm.DB,
	config *configuration.Data,
	clusterService cluster.Service,
	updateExecutor Executor,
	filterEnvType FilterEnvType,
	limitToCluster string) *TenantsUpdater {

	return &TenantsUpdater{
		db:             db,
		config:         config,
		clusterService: clusterService,
		updateExecutor: updateExecutor,
		filterEnvType:  filterEnvType,
		limitToCluster: limitToCluster,
	}
}

type TenantsUpdater struct {
	db             *gorm.DB
	config         *configuration.Data
	clusterService cluster.Service
	updateExecutor Executor
	filterEnvType  FilterEnvType
	limitToCluster string
}

type FilterEnvType interface {
	IsOk(envType environment.Type) bool
	GetLimit() string
}

var AllTypes = allTypes("no-limit")

type allTypes string

func (f allTypes) IsOk(envType environment.Type) bool {
	return true
}
func (f allTypes) GetLimit() string {
	return string(f)
}

type OneType environment.Type

func (f OneType) IsOk(envType environment.Type) bool {
	return envType == environment.Type(f)
}
func (f OneType) GetLimit() string {
	return string(f)
}

type FilterCluster func(cluster string) bool

func (u *TenantsUpdater) UpdateAllTenants() {

	log.Info(nil, map[string]interface{}{
		"env-types-limit": u.filterEnvType.GetLimit(),
		"cluster-limit":   u.limitToCluster,
	}, "triggering tenants update process")

	var followUp followUpFunc = func() error { return nil }

	prepareAndAssignStart := func(repo Repository, envTypes []environment.Type) error {
		log.Info(nil, map[string]interface{}{
			"env_types": envTypes,
		}, "starting update for outdated types")
		err := repo.PrepareForUpdating()
		if err != nil {
			return err
		}
		followUp = u.updateTenantsForTypes(envTypes)
		return nil
	}

	err := dbsupport.Transaction(u.db, lock(func(repo Repository) error {

		tenantUpdate, err := repo.GetTenantsUpdate()
		if err != nil {
			return err
		}
		if tenantUpdate.Status == Finished {
			log.Info(nil, map[string]interface{}{}, "last update was successfully finished")
			envTypesToUpdate, err := checkVersions(tenantUpdate)
			if err != nil {
				return err
			}
			if len(envTypesToUpdate) > 0 {
				return prepareAndAssignStart(repo, envTypesToUpdate)
			}
			log.Info(nil, map[string]interface{}{}, "there is nothing to be updated")

		} else if tenantUpdate.Status == Failed || tenantUpdate.Status == Killed || tenantUpdate.Status == Incomplete {
			log.Info(nil, map[string]interface{}{
				"failed_count": tenantUpdate.FailedCount,
			}, "last update has status \"%s\" - going to check failed or incomplete updates", tenantUpdate.Status)
			return prepareAndAssignStart(repo, environment.DefaultEnvTypes)

		} else if tenantUpdate.Status == Updating {
			if IsOlderThanTimeout(tenantUpdate.LastTimeUpdated, u.config) {
				return prepareAndAssignStart(repo, environment.DefaultEnvTypes)
			} else {
				log.Info(nil, map[string]interface{}{
					"automated_update_retry_sleep": u.config.GetAutomatedUpdateRetrySleep().String(),
				}, "there seems to be an ongoing update - going to wait for if the update still continues")
				followUp = u.waitAndRecheck
			}
		}

		return nil
	}))

	if err != nil {
		HandleTenantUpdateError(u.db, err)
		return
	}

	err = followUp()
	if err != nil {
		HandleTenantUpdateError(u.db, err)
	}
}

func HandleTenantUpdateError(db *gorm.DB, err error) {
	sentry.LogError(nil, map[string]interface{}{
		"commit": configuration.Commit,
		"err":    err,
	}, err, "automatic tenant update failed")
	err = dbsupport.Transaction(db, lock(func(repo Repository) error {
		return repo.UpdateStatus(Failed)
	}))
	if err != nil {
		sentry.LogError(nil, map[string]interface{}{
			"commit": configuration.Commit,
			"err":    err,
		}, err, "unable to set state to failed in tenants_update table")
	}
}

func (u *TenantsUpdater) waitAndRecheck() error {
	time.Sleep(u.config.GetAutomatedUpdateRetrySleep() + u.config.GetAutomatedUpdateRetrySleep()/10)

	followUp := func() error { return nil }

	err := dbsupport.Transaction(u.db, lock(func(repo Repository) error {

		tenantUpdate, err := repo.GetTenantsUpdate()
		if err != nil {
			return err
		}
		if IsOlderThanTimeout(tenantUpdate.LastTimeUpdated, u.config) {
			log.Info(nil, map[string]interface{}{}, "last update was interrupted - restarting a new one")
			err := repo.PrepareForUpdating()
			if err != nil {
				return err
			}
			followUp = u.updateTenantsForTypes(environment.DefaultEnvTypes)
		} else {
			log.Info(nil, map[string]interface{}{
				"last_time_updated": tenantUpdate.LastTimeUpdated,
			}, "there is still an ongoing update in process")
		}
		return nil
	}))

	if err != nil {
		return err
	}

	return followUp()
}

func IsOlderThanTimeout(when time.Time, config *configuration.Data) bool {
	return when.Before(time.Now().Add(-config.GetAutomatedUpdateRetrySleep()))
}

func (u *TenantsUpdater) updateTenantsForTypes(envTypes []environment.Type) followUpFunc {
	return func() error {
		mappedTemplates := environment.RetrieveMappedTemplates()
		typesWithVersion := map[environment.Type]string{}

		for _, envType := range envTypes {
			if u.filterEnvType.IsOk(envType) {
				typesWithVersion[envType] = mappedTemplates[envType].ConstructCompleteVersion()
			}
		}

		var clustersToUpdate []string
		if u.limitToCluster != "" {
			clustersToUpdate = []string{u.limitToCluster}
		} else {
			var err error
			clustersToUpdate, err = tenant.NewDBService(u.db).GetClustersToUpdate(typesWithVersion, configuration.Commit)
			if err != nil {
				return err
			}
		}
		errorChan := make(chan error, len(clustersToUpdate))
		wg := sync.WaitGroup{}
		wg.Add(len(clustersToUpdate))
		for _, cluster := range clustersToUpdate {
			go func(clusterURL string, typesAndVersion map[environment.Type]string, db *gorm.DB, config *configuration.Data, updateExecutor Executor) {
				defer wg.Done()
				err := updateForCluster(clusterURL, typesAndVersion, db, config, updateExecutor)
				if err != nil {
					errorChan <- err
					log.Error(nil, map[string]interface{}{
						"cluster_URL": clusterURL,
						"error":       err,
					}, "the tenants updated failed for the cluster")
				}
			}(cluster, typesWithVersion, u.db, u.config, u.updateExecutor)
		}
		wg.Wait()
		close(errorChan)
		errorMsg := utils.ListErrorsInMessage(errorChan, len(clustersToUpdate))
		if errorMsg != "" {
			return fmt.Errorf(errorMsg)
		}

		err := dbsupport.Transaction(u.db, lock(func(repo Repository) error {
			return u.setStatusAndVersionsAfterUpdate(repo)
		}))

		return err
	}
}

func updateForCluster(clusterURL string, typesWithVersion map[environment.Type]string, db *gorm.DB, config *configuration.Data, updateExecutor Executor) error {
	for {
		dbService := tenant.NewDBService(db)
		toUpdate, err := dbService.GetTenantsToUpdate(typesWithVersion, 100, configuration.Commit, clusterURL)
		if err != nil {
			return err
		}
		if len(toUpdate) == 0 {
			break
		}
		log.Info(nil, map[string]interface{}{
			"number_of_tenants_to_update": len(toUpdate),
			"master_url":                  clusterURL,
		}, "starting update for next batch of outdated/failed tenants")

		canContinue, err := updateTenants(toUpdate, typesWithVersion, db, config, updateExecutor)
		if err != nil {
			return err
		}
		if !canContinue {
			break
		}

		err = dbsupport.Transaction(db, lock(func(repo Repository) error {
			return repo.UpdateLastTimeUpdated()
		}))
		if err != nil {
			return err
		}
	}
	return nil
}

func (u *TenantsUpdater) setStatusAndVersionsAfterUpdate(repo Repository) error {
	tenantUpdate, err := repo.GetTenantsUpdate()
	if err != nil {
		return err
	}
	if !tenantUpdate.CanContinue {
		tenantUpdate.Status = Killed
	} else if tenantUpdate.FailedCount > 0 {
		tenantUpdate.Status = Failed
	} else if u.filterEnvType != AllTypes || u.limitToCluster != "" {
		tenantUpdate.Status = Incomplete
	} else {
		tenantUpdate.Status = Finished
	}
	for _, versionManager := range RetrieveVersionManagers() {
		isOk := true
		for _, envType := range versionManager.EnvTypes {
			isOk = isOk && u.filterEnvType.IsOk(envType)
		}
		if isOk {
			versionManager.SetCurrentVersion(tenantUpdate)
		}
	}
	log.Info(nil, map[string]interface{}{
		"status":                   tenantUpdate.Status,
		"number_of_failed_tenants": tenantUpdate.FailedCount,
	}, "the whole tenants update process has been finished")
	return repo.SaveTenantsUpdate(tenantUpdate)
}

func updateTenants(tenants []*tenant.Tenant, typesWithVersion map[environment.Type]string,
	db *gorm.DB, config *configuration.Data, updateExecutor Executor) (bool, error) {
	canContinue := true
	var err error

	for _, tnnt := range tenants {
		err := dbsupport.Transaction(db, func(tx *gorm.DB) error {
			var err error
			canContinue, err = NewRepository(tx).CanContinue()
			return err
		})
		if !canContinue || err != nil {
			log.Info(nil, map[string]interface{}{}, "stopping tenants update process")
			break
		}

		updateTenant(updateExecutor, tnnt, typesWithVersion, db)
		time.Sleep(config.GetAutomatedUpdateTimeGap())
	}
	return canContinue, err
}

func updateTenant(updateExecutor Executor, tnnt *tenant.Tenant, typesWithVersion map[environment.Type]string, db *gorm.DB) {
	namespaces, err := tenant.NewTenantRepository(db, tnnt.ID).GetNamespaces()
	if err != nil {
		sentry.LogError(nil, map[string]interface{}{
			"err":    err,
			"tenant": tnnt.ID,
		}, err, "unable to get current tenant namespaces during cluster-wide update")
		return
	}

	var envTypesToUpdate []environment.Type
	var nsNamesToUpdate []string
	for envType, version := range typesWithVersion {
		for _, ns := range namespaces {
			if ns.Type == envType && (ns.Version != version || ns.State == tenant.Failed) {
				envTypesToUpdate = append(envTypesToUpdate, envType)
				nsNamesToUpdate = append(nsNamesToUpdate, ns.Name)
				break
			}
		}
	}

	logParams := map[string]interface{}{
		"os_user":            tnnt.OSUsername,
		"tenant_id":          tnnt.ID,
		"ns_names_to_update": nsNamesToUpdate,
	}
	log.Info(nil, logParams, "starting update of tenant for outdated namespaces")
	err = updateExecutor.Update(nil, tnnt, nil, envTypesToUpdate, false)

	if err != nil {
		errIncr := dbsupport.Transaction(db, lock(func(repo Repository) error {
			return repo.IncrementFailedCount()
		}))
		if errIncr != nil {
			sentry.LogError(nil, map[string]interface{}{}, errIncr, "unable to increment failed_count")
		}
		sentry.LogError(nil, logParams, err, "unable to automatically update tenant")
	} else {
		log.Info(nil, logParams, "update of tenant for outdated namespace finished")
	}
}

func checkVersions(tu *TenantsUpdate) ([]environment.Type, error) {
	var types []environment.Type
	for _, versionManager := range RetrieveVersionManagers() {
		if !versionManager.IsVersionUpToDate(tu) {
			addIfNotPresent(&types, versionManager.EnvTypes)
		}
	}
	return types, nil
}

func addIfNotPresent(types *[]environment.Type, toAdd []environment.Type) {
	for _, toAddType := range toAdd {
		found := false
		for _, envType := range *types {
			if envType == toAddType {
				found = true
				break
			}
		}
		if !found {
			*types = append(*types, toAddType)
		}
	}
}

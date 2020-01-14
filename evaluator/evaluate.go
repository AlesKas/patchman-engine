package evaluator

import (
	"app/base"
	"app/base/database"
	"app/base/models"
	"app/base/utils"
	"context"
	"github.com/RedHatInsights/patchman-clients/vmaas"
	"github.com/antihax/optional"
	"github.com/jinzhu/gorm"
	"github.com/pkg/errors"
	"time"
)

const unknown = "unknown"

var (
	vmaasClient *vmaas.APIClient
)

func Configure() {
	traceAPI := utils.GetenvOrFail("LOG_LEVEL") == "trace"

	vmaasConfig := vmaas.NewConfiguration()
	vmaasConfig.BasePath = utils.GetenvOrFail("VMAAS_ADDRESS") + base.VMaaSAPIPrefix
	vmaasConfig.Debug = traceAPI
	vmaasClient = vmaas.NewAPIClient(vmaasConfig)
}

func Evaluate(ctx context.Context, systemID, rhAccountID int, updatesReq vmaas.UpdatesV3Request) {
	vmaasCallArgs := vmaas.AppUpdatesHandlerV3PostPostOpts{
		UpdatesV3Request: optional.NewInterface(updatesReq),
	}

	vmaasData, _, err := vmaasClient.UpdatesApi.AppUpdatesHandlerV3PostPost(ctx, &vmaasCallArgs)
	if err != nil {
		utils.Log("err", err.Error()).Error("Unable to get updates from VMaaS")
		return
	}

	tx := database.Db.Begin()
	err = processSystemAdvisories(tx, systemID, rhAccountID, vmaasData)
	if err != nil {
		tx.Rollback()
		utils.Log("err", err.Error()).Error("Unable to process system advisories")
		return
	}

	err = tx.Exec("SELECT * FROM update_system_caches(?)", systemID).Error
	if err != nil {
		tx.Rollback()
		utils.Log("err", err.Error()).Error("Unable to update system caches")
		return
	}

	err = tx.Model(&models.SystemPlatform{}).Where("id = ?", systemID).
		Update("last_evaluation", time.Now()).Error
	if err != nil {
		tx.Rollback()
		utils.Log("err", err.Error()).Error("Unable to update last_evaluation timestamp")
		return
	}

	tx.Commit()
}

func processSystemAdvisories(tx *gorm.DB, systemID, rhAccountID int, vmaasData vmaas.UpdatesV2Response) error {
	reported := getReportedAdvisories(vmaasData)
	stored, err := getStoredAdvisoriesMap(tx, systemID)
	if err != nil {
		return errors.Wrap(err, "Unable to get system stored advisories")
	}

	patched := getPatchedAdvisories(reported, *stored)
	newsAdvisoriesNames, unpatched := getNewAndUnpatchedAdvisories(reported, *stored)

	newIDs, err := ensureAdvisoriesInDb(tx, newsAdvisoriesNames)
	if err != nil {
		return errors.Wrap(err, "Unable to ensure new system advisories in db")
	}
	unpatched = append(unpatched, *newIDs...)

	err = updateSystemAdvisories(tx, systemID, rhAccountID, patched, unpatched)
	if err != nil {
		return errors.Wrap(err, "Unable to update system advisories")
	}
	return nil
}

func getReportedAdvisories(vmaasData vmaas.UpdatesV2Response) map[string]bool {
	advisories := map[string]bool{}
	for _, updates := range vmaasData.UpdateList {
		for _, update := range updates.AvailableUpdates {
			advisories[update.Erratum] = true
		}
	}
	return advisories
}

func getStoredAdvisoriesMap(tx *gorm.DB, systemID int) (*map[string]models.SystemAdvisories, error) {
	var advisories []models.SystemAdvisories
	err := database.SystemAdvisoriesQueryByID(tx, systemID).Preload("Advisory").Find(&advisories).Error
	if err != nil {
		return nil, err
	}

	advisoriesMap := map[string]models.SystemAdvisories{}
	for _, advisory := range advisories {
		advisoriesMap[advisory.Advisory.Name] = advisory
	}
	return &advisoriesMap, nil
}

func getNewAndUnpatchedAdvisories(reported map[string]bool, stored map[string]models.SystemAdvisories) (
	[]string, []int) {
	newAdvisories := []string{}
	unpatchedAdvisories := []int{}
	for reportedAdvisory := range reported {
		if storedAdvisory, found := stored[reportedAdvisory]; found {
			if storedAdvisory.WhenPatched != nil { // this advisory was already patched and now is un-patched again
				unpatchedAdvisories = append(unpatchedAdvisories, storedAdvisory.AdvisoryID)
			}
			utils.Log("advisory", storedAdvisory.Advisory.Name).Debug("still not patched")
		} else {
			newAdvisories = append(newAdvisories, reportedAdvisory)
		}
	}
	return newAdvisories, unpatchedAdvisories
}

func getPatchedAdvisories(reported map[string]bool, stored map[string]models.SystemAdvisories) []int {
	patchedAdvisories := make([]int, 0, len(stored))
	for storedAdvisory, storedAdvisoryObj := range stored {
		if _, found := reported[storedAdvisory]; found {
			continue
		}

		// advisory contained in reported - it's patched
		if storedAdvisoryObj.WhenPatched != nil {
			// it's already marked as patched
			continue
		}

		// advisory was patched from last evaluation, let's mark it as patched
		patchedAdvisories = append(patchedAdvisories, storedAdvisoryObj.AdvisoryID)
	}
	return patchedAdvisories
}

func updateSystemAdvisoriesWhenPatched(tx *gorm.DB, systemID, rhAccountID int, advisoryIDs []int,
	whenPatched *time.Time) error {
	err := tx.Model(models.SystemAdvisories{}).
		Where("system_id = ? AND advisory_id IN (?)", systemID, advisoryIDs).
		Update("when_patched", whenPatched).Error
	if err != nil {
		return err
	}

	affectedSystemIncrement := 0
	if whenPatched != nil {
		affectedSystemIncrement = -1
	} else {
		affectedSystemIncrement = 1
	}

	err = updateAccountAdvisoriesAffectedSystems(tx, rhAccountID, advisoryIDs, affectedSystemIncrement)
	return err
}

func updateAccountAdvisoriesAffectedSystems(tx *gorm.DB, rhAccountID int, advisoryIDs []int,
	affectedSystemIncrement int) error {
	err := tx.Model(models.AdvisoryAccountData{}).
		Where("rh_account_id = ? AND advisory_id IN (?)", rhAccountID, advisoryIDs).
		UpdateColumn("systems_affected", gorm.Expr("systems_affected + ?", affectedSystemIncrement)).Error
	return err
}

// Return advisory IDs, created advisories count, error
func ensureAdvisoriesInDb(tx *gorm.DB, advisories []string) (*[]int, error) {
	advisoryObjs := make(models.AdvisoryMetadataSlice, len(advisories))
	for i, advisory := range advisories {
		advisoryObjs[i] = models.AdvisoryMetadata{Name: advisory,
			Description: unknown, Synopsis: unknown, Summary: unknown, Solution: unknown}
	}

	txOnConflict := tx.Set("gorm:insert_option", "ON CONFLICT DO NOTHING")
	err := database.BulkInsert(txOnConflict, advisoryObjs)
	if err != nil {
		return nil, err
	}

	var advisoryIDs []int
	err = tx.Model(&models.AdvisoryMetadata{}).Where("name IN (?)", advisories).
		Pluck("id", &advisoryIDs).Error
	if err != nil {
		return nil, err
	}
	return &advisoryIDs, nil
}

func ensureSystemAdvisories(tx *gorm.DB, systemID int, advisoryIDs []int) error {
	advisoriesObjs := models.SystemAdvisoriesSlice{}
	for _, advisoryID := range advisoryIDs {
		advisoriesObjs = append(advisoriesObjs,
			models.SystemAdvisories{SystemID: systemID, AdvisoryID: advisoryID})
	}

	interfaceSlice := advisoriesObjs
	txOnConflict := tx.Set("gorm:insert_option", "ON CONFLICT DO NOTHING")
	err := database.BulkInsert(txOnConflict, interfaceSlice)
	return err
}

func ensureAdvisoryAccountDataInDb(tx *gorm.DB, rhAccountID int, advisoryIDs []int) error {
	accountData := make(models.AdvisoryAccountDataSlice, len(advisoryIDs))
	for i, advisoryID := range advisoryIDs {
		accountData[i] = models.AdvisoryAccountData{RhAccountID: rhAccountID, AdvisoryID: advisoryID}
	}

	txOnConflict := tx.Set("gorm:insert_option", "ON CONFLICT DO NOTHING")
	err := database.BulkInsert(txOnConflict, accountData)
	return err
}

func updateSystemAdvisories(tx *gorm.DB, systemID, rhAccountID int, patched, unpatched []int) error {
	whenPatched := time.Now()
	err := updateSystemAdvisoriesWhenPatched(tx, systemID, rhAccountID, patched, &whenPatched)
	if err != nil {
		return err
	}

	// delete items with no system related
	err = tx.Where("rh_account_id = ? AND systems_affected = 0", rhAccountID).
		Delete(&models.AdvisoryAccountData{}).Error
	if err != nil {
		return err
	}

	err = ensureSystemAdvisories(tx, systemID, unpatched)
	if err != nil {
		return err
	}

	err = ensureAdvisoryAccountDataInDb(tx, rhAccountID, unpatched)
	if err != nil {
		return err
	}

	err = updateSystemAdvisoriesWhenPatched(tx, systemID, rhAccountID, unpatched, nil)
	return err
}
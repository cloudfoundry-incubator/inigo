package volman_test

import (
	"encoding/json"
	"os"
	"time"

	"github.com/cloudfoundry-incubator/auction/auctiontypes"
	"github.com/cloudfoundry-incubator/bbs"
	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/inigo/helpers"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/ifrit/grouper"
)

var _ = Describe("Tasks", func() {
	var (
		cellProcess, plumbing ifrit.Process
		fileServerStaticDir   string
		logger                lager.Logger
		bbsClient             bbs.InternalClient
	)

	BeforeEach(func() {
		var fileServerRunner ifrit.Runner
		fileServerRunner, fileServerStaticDir = componentMaker.FileServer()

		plumbing = ginkgomon.Invoke(grouper.NewOrdered(os.Kill, grouper.Members{
			{"initial-services", grouper.NewParallel(os.Kill, grouper.Members{
				{"etcd", componentMaker.Etcd()},
				{"consul", componentMaker.Consul()},
			})},
			{"bbs", componentMaker.BBS()},
		}))

		helpers.ConsulWaitUntilReady()

		cellProcess = ginkgomon.Invoke(grouper.NewParallel(os.Interrupt, grouper.Members{
			{"file-server", fileServerRunner},
			{"rep", componentMaker.Rep("-memoryMB", "1024")},
			{"auctioneer", componentMaker.Auctioneer()},
		}))

		bbsServiceClient := componentMaker.BBSServiceClient(logger)
		bbsClient = componentMaker.BBSClient()

		Eventually(func() (models.CellSet, error) { return bbsServiceClient.Cells(logger) }).Should(HaveLen(1))
	})

	AfterEach(func() {
		helpers.StopProcesses(plumbing, cellProcess)
	})

	Describe("running a task with volume mount", func() {
		var (
			fileName, guid string
		)

		Context("when driver required is available on a cell", func() {
			BeforeEach(func() {
				guid = helpers.GenerateGuid()

				fileName = "testfile-" + string(time.Now().UnixNano()) + ".txt"
				expectedTask := helpers.TaskCreateRequest(
					guid,
					&models.RunAction{
						Path: "/bin/touch",
						User: "root",
						Args: []string{"/testmount/" + fileName},
					},
				)
				expectedTask.VolumeMounts = []*models.VolumeMount{
					generateVolumeObject("fakedriver"),
				}

				err := bbsClient.DesireTask(expectedTask.TaskGuid, expectedTask.Domain, expectedTask.TaskDefinition)
				Expect(err).NotTo(HaveOccurred())
			})

			It("can write files to the mounted volume", func() {
				var task *models.Task
				Eventually(func() interface{} {
					var err error

					task, err = bbsClient.TaskByGuid(guid)
					Expect(err).NotTo(HaveOccurred())

					return task.State
				}).Should(Equal(models.Task_Completed))

				Expect(task.Failed).To(BeFalse())
			})
		})

		Context("when driver required not available on any cell", func() {
			var (
				expectedTask *models.Task
			)

			BeforeEach(func() {
				guid = helpers.GenerateGuid()
				expectedTask = helpers.TaskCreateRequest(
					guid,
					&models.RunAction{
						User: "vcap",
						Path: "sh",
						Args: []string{"-c", "echo 'volman!'"},
					},
				)
				expectedTask.VolumeMounts = []*models.VolumeMount{
					generateVolumeObject("non-existent-driver"),
				}
			})

			It("should error placing the task", func() {
				err := bbsClient.DesireTask(expectedTask.TaskGuid, expectedTask.Domain, expectedTask.TaskDefinition)
				Expect(err).NotTo(HaveOccurred())

				var task *models.Task
				Eventually(func() interface{} {
					var err error

					task, err = bbsClient.TaskByGuid(expectedTask.TaskGuid)
					Expect(err).NotTo(HaveOccurred())

					return task.State
				}).Should(Equal(models.Task_Completed))

				Expect(task.Failed).To(BeTrue())
				Expect(task.FailureReason).To(Equal(auctiontypes.ErrorCellMismatch.Error()))
			})
		})

		Context("when one of the drivers required is not available on any cell", func() {
			var (
				expectedTask *models.Task
			)

			BeforeEach(func() {
				guid = helpers.GenerateGuid()
				expectedTask = helpers.TaskCreateRequest(
					guid,
					&models.RunAction{
						User: "vcap",
						Path: "sh",
						Args: []string{"-c", "echo 'volman!'"},
					},
				)

				expectedTask.VolumeMounts = []*models.VolumeMount{
					generateVolumeObject("non-existent-driver"),
					generateVolumeObject("fakedriver"),
				}
			})

			It("should error placing the task", func() {
				err := bbsClient.DesireTask(expectedTask.TaskGuid, expectedTask.Domain, expectedTask.TaskDefinition)
				Expect(err).NotTo(HaveOccurred())

				var task *models.Task
				Eventually(func() interface{} {
					var err error

					task, err = bbsClient.TaskByGuid(expectedTask.TaskGuid)
					Expect(err).NotTo(HaveOccurred())

					return task.State
				}).Should(Equal(models.Task_Completed))

				Expect(task.Failed).To(BeTrue())
				Expect(task.FailureReason).To(Equal(auctiontypes.ErrorCellMismatch.Error()))
			})
		})
	})
})

func generateVolumeObject(driver string) *models.VolumeMount {
	volumeId := "some-volumeID-" + string(time.Now().UnixNano())
	someConfig := map[string]interface{}{"volume_id": volumeId}
	jsonSomeConfig, err := json.Marshal(someConfig)
	Expect(err).NotTo(HaveOccurred())

	return &models.VolumeMount{
		Driver:        driver,
		VolumeId:      volumeId,
		ContainerPath: "/testmount",
		Mode:          models.BindMountMode_RW,
		Config:        jsonSomeConfig,
	}
}

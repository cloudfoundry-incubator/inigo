package cell_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cloudfoundry-incubator/bbs/models"
	"github.com/cloudfoundry-incubator/inigo/fixtures"
	"github.com/cloudfoundry-incubator/inigo/helpers"
	"github.com/cloudfoundry-incubator/rep"
	"github.com/cloudfoundry-incubator/routing-info/cfroutes"
	"github.com/cloudfoundry-incubator/stager/diego_errors"
	archive_helper "github.com/pivotal-golang/archiver/extractor/test_helper"
	"github.com/pivotal-golang/lager"
	"github.com/pivotal-golang/lager/lagertest"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/ginkgomon"
	"github.com/tedsuo/ifrit/grouper"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("LRP", func() {
	var (
		processGuid         string
		archiveFiles        []archive_helper.ArchiveFile
		fileServerStaticDir string

		runtime ifrit.Process
	)

	BeforeEach(func() {
		processGuid = helpers.GenerateGuid()

		var fileServer ifrit.Runner
		fileServer, fileServerStaticDir = componentMaker.FileServer()
		runtime = ginkgomon.Invoke(grouper.NewParallel(os.Kill, grouper.Members{
			{"router", componentMaker.Router()},
			{"file-server", fileServer},
			{"rep", componentMaker.Rep()},
			{"converger", componentMaker.Converger()},
			{"auctioneer", componentMaker.Auctioneer()},
			{"route-emitter", componentMaker.RouteEmitter()},
		}))

		archiveFiles = fixtures.GoServerApp()
	})

	JustBeforeEach(func() {
		archive_helper.CreateZipArchive(
			filepath.Join(fileServerStaticDir, "lrp.zip"),
			archiveFiles,
		)
	})

	AfterEach(func() {
		helpers.StopProcesses(runtime)
	})

	Describe("desiring", func() {
		var lrp *models.DesiredLRP

		BeforeEach(func() {
			lrp = helpers.DefaultLRPCreateRequest(processGuid, "log-guid", 1)
			lrp.Setup = nil
			lrp.CachedDependencies = []*models.CachedDependency{{
				From:      fmt.Sprintf("http://%s/v1/static/%s", componentMaker.Addresses.FileServer, "lrp.zip"),
				To:        "/tmp/diego/lrp",
				Name:      "lrp bits",
				CacheKey:  "lrp-cache-key",
				LogSource: "APP",
			}}
			lrp.LegacyDownloadUser = "vcap"
			lrp.Privileged = false
			lrp.Action = models.WrapAction(&models.RunAction{
				User: "vcap",
				Path: "/tmp/diego/lrp/go-server",
				Env:  []*models.EnvironmentVariable{{"PORT", "8080"}},
			})
		})

		JustBeforeEach(func() {
			err := bbsClient.DesireLRP(lrp)
			Expect(err).NotTo(HaveOccurred())
		})

		It("eventually runs", func() {
			Eventually(helpers.LRPStatePoller(bbsClient, processGuid, nil)).Should(Equal(models.ActualLRPStateRunning))
			Eventually(helpers.HelloWorldInstancePoller(componentMaker.Addresses.Router, helpers.DefaultHost)).Should(ConsistOf([]string{"0"}))
		})

		Context("when properties are present on the desired LRP", func() {
			BeforeEach(func() {
				lrp.Properties = map[string]string{
					"my-key": "my-value",
				}
			})

			It("passes them to garden", func() {
				Eventually(helpers.LRPStatePoller(bbsClient, processGuid, nil)).Should(Equal(models.ActualLRPStateRunning))

				lrps, err := bbsClient.ActualLRPGroupsByProcessGuid(processGuid)
				Expect(err).NotTo(HaveOccurred())

				actualLRP := lrps[0].Instance
				containerHandle := fmt.Sprintf("%s-%s", actualLRP.ProcessGuid, actualLRP.InstanceGuid)

				container, err := gardenClient.Lookup(containerHandle)
				Expect(err).NotTo(HaveOccurred())

				props, err := container.Properties()
				Expect(err).NotTo(HaveOccurred())

				Expect(props).To(HaveKeyWithValue("my-key", "my-value"))
			})
		})

		Context("when it's unhealthy for longer than its start timeout", func() {
			BeforeEach(func() {
				lrp.StartTimeout = 5

				lrp.Monitor = models.WrapAction(&models.RunAction{
					User: "vcap",
					Path: "false",
				})
			})

			It("eventually marks the LRP as crashed", func() {
				Eventually(
					helpers.LRPStatePoller(bbsClient, processGuid, nil),
				).Should(Equal(models.ActualLRPStateCrashed))
			})
		})

		Describe("updating routes", func() {
			BeforeEach(func() {
				lrp.Ports = []uint32{8080, 9080}
				routes := cfroutes.CFRoutes{{Hostnames: []string{"lrp-route-8080"}, Port: 8080}}.RoutingInfo()
				lrp.Routes = &routes

				lrp.Action = models.WrapAction(&models.RunAction{
					User: "vcap",
					Path: "/tmp/diego/lrp/go-server",
					Env:  []*models.EnvironmentVariable{{"PORT", "8080 9080"}},
				})
			})

			It("can not access container ports without routes", func() {
				Eventually(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses.Router, "lrp-route-8080")).Should(Equal(http.StatusOK))
				Consistently(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses.Router, "lrp-route-8080")).Should(Equal(http.StatusOK))
				Consistently(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses.Router, "lrp-route-9080")).Should(Equal(http.StatusNotFound))
			})

			Context("when adding a route", func() {
				logger := lagertest.NewTestLogger("test")

				JustBeforeEach(func() {
					routes := cfroutes.CFRoutes{
						{Hostnames: []string{"lrp-route-8080"}, Port: 8080},
						{Hostnames: []string{"lrp-route-9080"}, Port: 9080},
					}.RoutingInfo()

					desiredUpdate := models.DesiredLRPUpdate{
						Routes: &routes,
					}

					logger.Info("just-before-each", lager.Data{
						"processGuid":   processGuid,
						"updateRequest": desiredUpdate})

					err := bbsClient.UpdateDesiredLRP(processGuid, &desiredUpdate)

					logger.Info("after-update-desired", lager.Data{
						"error": err,
					})
					Expect(err).NotTo(HaveOccurred())
				})

				It("can immediately access the container port with the associated routes", func() {
					Eventually(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses.Router, "lrp-route-8080")).Should(Equal(http.StatusOK))
					Consistently(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses.Router, "lrp-route-8080")).Should(Equal(http.StatusOK))

					Eventually(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses.Router, "lrp-route-9080")).Should(Equal(http.StatusOK))
					Consistently(helpers.ResponseCodeFromHostPoller(componentMaker.Addresses.Router, "lrp-route-9080")).Should(Equal(http.StatusOK))
				})
			})
		})

		Describe("when started with 2 instances", func() {
			BeforeEach(func() {
				lrp.Instances = 2
			})

			JustBeforeEach(func() {
				Eventually(func() []*models.ActualLRPGroup {
					lrps, err := bbsClient.ActualLRPGroupsByProcessGuid(processGuid)
					Expect(err).NotTo(HaveOccurred())

					return lrps
				}).Should(HaveLen(2))
				Eventually(helpers.HelloWorldInstancePoller(componentMaker.Addresses.Router, helpers.DefaultHost)).Should(ConsistOf([]string{"0", "1"}))
			})

			Describe("changing the instances", func() {
				var newInstances int32

				BeforeEach(func() {
					newInstances = 0 // base value; overridden below
				})

				JustBeforeEach(func() {
					err := bbsClient.UpdateDesiredLRP(processGuid, &models.DesiredLRPUpdate{
						Instances: &newInstances,
					})
					Expect(err).NotTo(HaveOccurred())
				})

				Context("scaling it up to 3", func() {
					BeforeEach(func() {
						newInstances = 3
					})

					It("scales up to the correct number of instances", func() {
						Eventually(func() []*models.ActualLRPGroup {
							lrps, err := bbsClient.ActualLRPGroupsByProcessGuid(processGuid)
							Expect(err).NotTo(HaveOccurred())

							return lrps
						}).Should(HaveLen(3))

						Eventually(helpers.HelloWorldInstancePoller(componentMaker.Addresses.Router, helpers.DefaultHost)).Should(ConsistOf([]string{"0", "1", "2"}))
					})
				})

				Context("scaling it down to 1", func() {
					BeforeEach(func() {
						newInstances = 1
					})

					It("scales down to the correct number of instances", func() {
						Eventually(func() []*models.ActualLRPGroup {
							lrps, err := bbsClient.ActualLRPGroupsByProcessGuid(processGuid)
							Expect(err).NotTo(HaveOccurred())

							return lrps
						}).Should(HaveLen(1))

						Eventually(helpers.HelloWorldInstancePoller(componentMaker.Addresses.Router, helpers.DefaultHost)).Should(ConsistOf([]string{"0"}))
					})
				})

				Context("scaling it down to 0", func() {
					BeforeEach(func() {
						newInstances = 0
					})

					It("scales down to the correct number of instances", func() {
						Eventually(func() []*models.ActualLRPGroup {
							lrps, err := bbsClient.ActualLRPGroupsByProcessGuid(processGuid)
							Expect(err).NotTo(HaveOccurred())

							return lrps
						}).Should(BeEmpty())

						Eventually(helpers.HelloWorldInstancePoller(componentMaker.Addresses.Router, helpers.DefaultHost)).Should(BeEmpty())
					})

					It("can be scaled back up", func() {
						newInstances := int32(1)
						err := bbsClient.UpdateDesiredLRP(processGuid, &models.DesiredLRPUpdate{
							Instances: &newInstances,
						})
						Expect(err).NotTo(HaveOccurred())

						Eventually(func() []*models.ActualLRPGroup {
							lrps, err := bbsClient.ActualLRPGroupsByProcessGuid(processGuid)
							Expect(err).NotTo(HaveOccurred())

							return lrps
						}).Should(HaveLen(1))

						Eventually(helpers.HelloWorldInstancePoller(componentMaker.Addresses.Router, helpers.DefaultHost)).Should(ConsistOf([]string{"0"}))
					})
				})
			})

			Describe("deleting it", func() {
				JustBeforeEach(func() {
					err := bbsClient.RemoveDesiredLRP(processGuid)
					Expect(err).NotTo(HaveOccurred())
				})

				It("stops all instances", func() {
					Eventually(func() []*models.ActualLRPGroup {
						lrps, err := bbsClient.ActualLRPGroupsByProcessGuid(processGuid)
						Expect(err).NotTo(HaveOccurred())

						return lrps
					}).Should(BeEmpty())

					Eventually(helpers.HelloWorldInstancePoller(componentMaker.Addresses.Router, helpers.DefaultHost)).Should(BeEmpty())
				})
			})
		})

		Context("Egress Rules", func() {
			BeforeEach(func() {
				archiveFiles = fixtures.CurlLRP()
				lrp.Action = models.WrapAction(&models.RunAction{
					User: "vcap",
					Path: "bash",
					Args: []string{"server.sh"},
					Env:  []*models.EnvironmentVariable{{"PORT", "8080"}},
				})

				lrp.Setup = models.WrapAction(&models.DownloadAction{
					From: fmt.Sprintf("http://%s/v1/static/%s", componentMaker.Addresses.FileServer, "lrp.zip"),
					To:   ".",
					User: "vcap",
				})

				lrp.CachedDependencies = nil
			})

			Context("default networking", func() {
				It("rejects outbound tcp traffic", func() {
					Eventually(func() string {
						bytes, statusCode, err := helpers.ResponseBodyAndStatusCodeFromHost(componentMaker.Addresses.Router, helpers.DefaultHost)
						if err != nil {
							return err.Error()
						}
						if statusCode != http.StatusOK {
							return strconv.Itoa(statusCode)
						}

						return string(bytes)
					}).Should(Equal("28"))
				})
			})

			Context("with appropriate security group setting", func() {
				BeforeEach(func() {
					lrp.EgressRules = []*models.SecurityGroupRule{
						{
							Protocol:     models.TCPProtocol,
							Destinations: []string{"9.0.0.0-89.255.255.255", "90.0.0.0-94.0.0.0"},
							Ports:        []uint32{80},
						},
						{
							Protocol:     models.UDPProtocol,
							Destinations: []string{"0.0.0.0/0"},
							PortRange: &models.PortRange{
								Start: 53,
								End:   53,
							},
						},
					}
				})

				It("allows outbound tcp traffic", func() {
					Eventually(func() string {
						bytes, statusCode, err := helpers.ResponseBodyAndStatusCodeFromHost(componentMaker.Addresses.Router, helpers.DefaultHost)
						if err != nil {
							return err.Error()
						}
						if statusCode != http.StatusOK {
							return strconv.Itoa(statusCode)
						}

						return string(bytes)
					}).Should(Equal("0"))
				})
			})
		})

		Context("Unsupported preloaded rootfs is requested", func() {
			BeforeEach(func() {
				lrp = helpers.LRPCreateRequestWithRootFS(processGuid, helpers.BogusPreloadedRootFS)
			})

			It("fails and sets a placement error", func() {
				lrpFunc := func() string {
					lrpGroups, err := bbsClient.ActualLRPGroupsByProcessGuid(processGuid)
					Expect(err).NotTo(HaveOccurred())
					if len(lrpGroups) == 0 {
						return ""
					}
					lrp, _ := lrpGroups[0].Resolve()

					return lrp.PlacementError
				}

				Eventually(lrpFunc).Should(Equal(diego_errors.CELL_MISMATCH_MESSAGE))
			})
		})

		Context("Unsupported arbitrary rootfs is requested", func() {
			BeforeEach(func() {
				lrp = helpers.LRPCreateRequestWithRootFS(processGuid, "socker://hello")
			})

			It("fails and sets a placement error", func() {
				lrpFunc := func() string {
					lrpGroups, err := bbsClient.ActualLRPGroupsByProcessGuid(processGuid)
					Expect(err).NotTo(HaveOccurred())
					if len(lrpGroups) == 0 {
						return ""
					}
					lrp, _ := lrpGroups[0].Resolve()

					return lrp.PlacementError
				}

				Eventually(lrpFunc).Should(Equal(diego_errors.CELL_MISMATCH_MESSAGE))
			})
		})

		Context("Supported arbitrary rootfs scheme (viz., docker) is requested", func() {
			BeforeEach(func() {
				// docker is supported
				lrp = helpers.DockerLRPCreateRequest(processGuid)
			})

			It("runs", func() {
				Eventually(func() []*models.ActualLRPGroup {
					lrps, err := bbsClient.ActualLRPGroupsByProcessGuid(processGuid)
					Expect(err).NotTo(HaveOccurred())
					return lrps
				}).Should(HaveLen(1))

				poller := helpers.HelloWorldInstancePoller(componentMaker.Addresses.Router, helpers.DefaultHost)
				Eventually(poller).Should(ConsistOf([]string{"0"}))
			})
		})
	})
})

var _ = Describe("Crashing LRPs", func() {
	var (
		processGuid string
		runtime     ifrit.Process
	)

	crashCount := func(guid string, index int) func() int32 {
		return func() int32 {
			actualGroup, err := bbsClient.ActualLRPGroupByProcessGuidAndIndex(guid, index)
			Expect(err).NotTo(HaveOccurred())
			actual, _ := actualGroup.Resolve()
			return actual.CrashCount
		}
	}

	BeforeEach(func() {
		fileServer, fileServerStaticDir := componentMaker.FileServer()

		archiveFiles := fixtures.HelloWorldIndexLRP()
		archive_helper.CreateZipArchive(
			filepath.Join(fileServerStaticDir, "lrp.zip"),
			archiveFiles,
		)

		processGuid = helpers.GenerateGuid()

		runtime = ginkgomon.Invoke(grouper.NewParallel(os.Kill, grouper.Members{
			{"router", componentMaker.Router()},
			{"file-server", fileServer},
			{"rep", componentMaker.Rep()},
			{"converger", componentMaker.Converger(
				"-convergeRepeatInterval", "1s",
			)},
			{"auctioneer", componentMaker.Auctioneer()},
			{"route-emitter", componentMaker.RouteEmitter()},
		}))
	})

	AfterEach(func() {
		helpers.StopProcesses(runtime)
	})

	Describe("crashing apps", func() {
		Context("when an app flaps", func() {
			BeforeEach(func() {
				lrp := helpers.CrashingLRPCreateRequest(processGuid)

				err := bbsClient.DesireLRP(lrp)
				Expect(err).NotTo(HaveOccurred())
			})

			It("imediately restarts the app 3 times", func() {
				// the bbs immediately starts it 3 times
				Eventually(crashCount(processGuid, 0)).Should(BeEquivalentTo(3))
				// then exponential backoff kicks in
				Consistently(crashCount(processGuid, 0), 15*time.Second).Should(BeEquivalentTo(3))
				// eventually we cross the first backoff threshold (30 seconds)
				Eventually(crashCount(processGuid, 0), 30*time.Second).Should(BeEquivalentTo(4))
			})
		})
	})

	Describe("disappearing containrs", func() {
		Context("when a container is deleted unexpectedly", func() {
			BeforeEach(func() {
				lrp := helpers.DefaultLRPCreateRequest(processGuid, "log-guid", 1)

				err := bbsClient.DesireLRP(lrp)
				Expect(err).NotTo(HaveOccurred())

				Eventually(helpers.LRPStatePoller(bbsClient, processGuid, nil)).Should(Equal(models.ActualLRPStateRunning))
			})

			It("crashes the instance and restarts it", func() {
				group, err := bbsClient.ActualLRPGroupByProcessGuidAndIndex(processGuid, 0)
				Expect(err).NotTo(HaveOccurred())

				handle := rep.LRPContainerGuid(group.Instance.GetProcessGuid(), group.Instance.GetInstanceGuid())
				err = gardenClient.Destroy(handle)
				Expect(err).NotTo(HaveOccurred())

				Eventually(crashCount(processGuid, 0)).Should(BeEquivalentTo(1))
				Eventually(helpers.LRPStatePoller(bbsClient, processGuid, nil)).Should(Equal(models.ActualLRPStateRunning))
			})
		})
	})
})

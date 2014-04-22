package inigo_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"

	"github.com/cloudfoundry-incubator/inigo/loggredile"
	"github.com/fraenkel/candiedyaml"

	"github.com/cloudfoundry-incubator/inigo/inigo_server"
	"github.com/cloudfoundry-incubator/inigo/stager_runner"
	"github.com/cloudfoundry-incubator/runtime-schema/models/factories"
	"github.com/cloudfoundry/yagnats"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	zip_helper "github.com/pivotal-golang/archiver/extractor/test_helper"
)

func downloadBuildArtifactsCache(appId string) []byte {
	fileServerUrl := fmt.Sprintf("http://127.0.0.1:%d/build_artifacts/%s", suiteContext.FileServerPort, appId)
	resp, err := http.Get(fileServerUrl)
	Ω(err).ShouldNot(HaveOccurred())

	Ω(resp.StatusCode).Should(Equal(http.StatusOK))

	bytes, err := ioutil.ReadAll(resp.Body)
	Ω(err).ShouldNot(HaveOccurred())

	return bytes
}

var _ = Describe("Stager", func() {
	var otherStagerRunner *stager_runner.StagerRunner
	var appId = "some-app-id"

	BeforeEach(func() {
		suiteContext.FileServerRunner.Start()
		otherStagerRunner = stager_runner.New(
			suiteContext.SharedContext.StagerPath,
			suiteContext.EtcdRunner.NodeURLS(),
			[]string{fmt.Sprintf("127.0.0.1:%d", suiteContext.NatsPort)},
		)
	})

	Context("when unable to find an appropriate compiler", func() {
		BeforeEach(func() {
			suiteContext.ExecutorRunner.Start()
			suiteContext.StagerRunner.Start()
		})

		It("returns an error", func() {
			receivedMessages := make(chan *yagnats.Message)
			suiteContext.NatsRunner.MessageBus.Subscribe("compiler-stagers-test", func(message *yagnats.Message) {
				receivedMessages <- message
			})

			suiteContext.NatsRunner.MessageBus.PublishWithReplyTo(
				"diego.staging.start",
				"compiler-stagers-test",
				[]byte(fmt.Sprintf(`{
					"app_id": "%s",
					"task_id": "some-task-id",
					"app_bits_download_uri": "some-download-uri",
					"build_artifacts_cache_download_uri": "artifacts-download-uri",
					"build_artifacts_cache_upload_uri": "artifacts-upload-uri",
					"stack": "no-compiler"
				}`, appId)),
			)

			var receivedMessage *yagnats.Message
			Eventually(receivedMessages, SHORT_TIMEOUT).Should(Receive(&receivedMessage))
			Ω(receivedMessage.Payload).Should(ContainSubstring("no compiler defined for requested stack"))
			Consistently(receivedMessages, SHORT_TIMEOUT).ShouldNot(Receive())
		})
	})

	Describe("Staging", func() {
		var outputGuid string
		var stagingMessage []byte
		var buildpackToUse string

		BeforeEach(func() {
			suiteContext.ExecutorRunner.Start()

			buildpackToUse = "admin_buildpack.zip"
			outputGuid = factories.GenerateGuid()

			suiteContext.FileServerRunner.ServeFile("smelter.zip", suiteContext.SharedContext.SmelterZipPath)

			//make and upload an app
			var appFiles = []zip_helper.ArchiveFile{
				{Name: "my-app", Body: "scooby-doo"},
			}

			zip_helper.CreateZipArchive("/tmp/app.zip", appFiles)
			inigo_server.UploadFile("app.zip", "/tmp/app.zip")

			//make and upload a buildpack
			var adminBuildpackFiles = []zip_helper.ArchiveFile{
				{
					Name: "bin/detect",
					Body: `#!/bin/bash
echo My Buildpack
				`},
				{
					Name: "bin/compile",
					Body: `#!/bin/bash
echo $1 $2
echo COMPILING BUILDPACK
echo $SOME_STAGING_ENV
touch $1/compiled
touch $2/inserted-into-artifacts-cache
				`},
				{
					Name: "bin/release",
					Body: `#!/bin/bash
cat <<EOF
---
default_process_types:
  web: start-command
EOF
				`},
			}
			zip_helper.CreateZipArchive("/tmp/admin_buildpack.zip", adminBuildpackFiles)
			inigo_server.UploadFile("admin_buildpack.zip", "/tmp/admin_buildpack.zip")

			var bustedAdminBuildpackFiles = []zip_helper.ArchiveFile{
				{
					Name: "bin/detect",
					Body: `#!/bin/bash]
				exit 1
				`},
				{Name: "bin/compile", Body: `#!/bin/bash`},
				{Name: "bin/release", Body: `#!/bin/bash`},
			}

			zip_helper.CreateZipArchive("/tmp/busted_admin_buildpack.zip", bustedAdminBuildpackFiles)
			inigo_server.UploadFile("busted_admin_buildpack.zip", "/tmp/busted_admin_buildpack.zip")
		})

		JustBeforeEach(func() {
			stagingMessage = []byte(
				fmt.Sprintf(
					`{
						"app_id": "%s",
						"task_id": "some-task-id",
						"memory_mb": 128,
						"disk_mb": 128,
						"file_descriptors": 1024,
						"stack": "default",
						"app_bits_download_uri": "%s",
						"buildpacks" : [{ "key": "test-buildpack", "url": "%s" }],
						"environment": [{ "key": "SOME_STAGING_ENV", "value": "%s"}]
					}`,
					appId,
					inigo_server.DownloadUrl("app.zip"),
					inigo_server.DownloadUrl(buildpackToUse),
					outputGuid,
				),
			)
		})

		Context("with one stager running", func() {
			BeforeEach(func() {
				suiteContext.StagerRunner.Start("--compilers", `{"default":"smelter.zip"}`)
			})

			It("runs the compiler on the executor with the correct environment variables, bits and log tag, and responds with the detected buildpack", func() {
				//listen for NATS response
				payloads := make(chan []byte)

				suiteContext.NatsRunner.MessageBus.Subscribe("stager-test", func(msg *yagnats.Message) {
					payloads <- msg.Payload
				})

				//stream logs
				messages, stop := loggredile.StreamMessages(
					suiteContext.LoggregatorRunner.Config.OutgoingPort,
					fmt.Sprintf("/tail/?app=%s", appId),
				)
				defer close(stop)

				logOutput := gbytes.NewBuffer()
				go func() {
					for message := range messages {
						Ω(message.GetSourceName()).To(Equal("STG"))
						logOutput.Write([]byte(string(message.GetMessage()) + "\n"))
					}
				}()

				//publish the staging message
				err := suiteContext.NatsRunner.MessageBus.PublishWithReplyTo(
					"diego.staging.start",
					"stager-test",
					stagingMessage,
				)
				Ω(err).ShouldNot(HaveOccurred())

				//wait for staging to complete
				var payload []byte
				Eventually(payloads, LONG_TIMEOUT).Should(Receive(&payload))

				//Assert on the staging output (detected buildpack)
				Ω(string(payload)).Should(MatchJSON(`{
					"buildpack_key":"test-buildpack",
					"detected_buildpack":"My Buildpack"
				}`))

				//Asser the user saw reasonable output
				Eventually(logOutput).Should(gbytes.Say("COMPILING BUILDPACK"))
				Ω(logOutput.Contents()).Should(ContainSubstring(outputGuid))

				// Assert that the build artifacts cache was downloaded
				//TODO: how do we test they were downloaded??

				// Download the build artifacts cache from the file-server
				buildArtifactsCacheBytes := downloadBuildArtifactsCache(appId)
				Ω(buildArtifactsCacheBytes).ShouldNot(BeEmpty())

				// Assert that the downloaded build artifacts cache matches what the buildpack created
				artifactsCache, err := gzip.NewReader(bytes.NewReader(buildArtifactsCacheBytes))
				Ω(err).ShouldNot(HaveOccurred())

				untarredBuildArtifactsData := tar.NewReader(artifactsCache)
				buildArtifactContents := map[string][]byte{}
				for {
					hdr, err := untarredBuildArtifactsData.Next()
					if err == io.EOF {
						break
					}

					Ω(err).ShouldNot(HaveOccurred())

					content, err := ioutil.ReadAll(untarredBuildArtifactsData)
					Ω(err).ShouldNot(HaveOccurred())

					buildArtifactContents[hdr.Name] = content
				}

				//Ω(buildArtifactContents).Should(HaveKey("pulled-down-from-artifacts-cache"))
				Ω(buildArtifactContents).Should(HaveKey("inserted-into-artifacts-cache"))

				//Fetch the compiled droplet from the fakeCC
				dropletData, ok := suiteContext.FakeCC.UploadedDroplets[appId]
				Ω(ok).Should(BeTrue())
				Ω(dropletData).ShouldNot(BeEmpty())

				//Unzip the droplet
				ungzippedDropletData, err := gzip.NewReader(bytes.NewReader(dropletData))
				Ω(err).ShouldNot(HaveOccurred())

				//Untar the droplet
				untarredDropletData := tar.NewReader(ungzippedDropletData)
				dropletContents := map[string][]byte{}
				for {
					hdr, err := untarredDropletData.Next()
					if err == io.EOF {
						break
					}
					Ω(err).ShouldNot(HaveOccurred())

					content, err := ioutil.ReadAll(untarredDropletData)
					Ω(err).ShouldNot(HaveOccurred())

					dropletContents[hdr.Name] = content
				}

				//Assert the droplet has the right files in it
				Ω(dropletContents).Should(HaveKey("staging_info.yml"))
				Ω(dropletContents).Should(HaveKey("logs/"))
				Ω(dropletContents).Should(HaveKey("tmp/"))
				Ω(dropletContents).Should(HaveKey("app/"))
				Ω(dropletContents).Should(HaveKey("app/my-app"))
				Ω(dropletContents).Should(HaveKey("app/compiled"))

				//Assert the files contain the right content
				Ω(string(dropletContents["app/my-app"])).Should(Equal("scooby-doo"))

				//In particular, staging_info.yml should have the correct detected_buildpack and start_command
				yamlDecoder := candiedyaml.NewDecoder(bytes.NewReader(dropletContents["staging_info.yml"]))
				stagingInfo := map[string]string{}
				err = yamlDecoder.Decode(&stagingInfo)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(stagingInfo["detected_buildpack"]).Should(Equal("My Buildpack"))
				Ω(stagingInfo["start_command"]).Should(Equal("start-command"))

				//Assert nothing else crept into the droplet
				Ω(dropletContents).Should(HaveLen(6))
			})

			Context("when compilation fails", func() {
				BeforeEach(func() {
					buildpackToUse = "busted_admin_buildpack.zip"
				})

				It("responds with the error, and no detected buildpack present", func() {
					payloads := make(chan []byte)

					suiteContext.NatsRunner.MessageBus.Subscribe("stager-test", func(msg *yagnats.Message) {
						payloads <- msg.Payload
					})

					messages, stop := loggredile.StreamMessages(
						suiteContext.LoggregatorRunner.Config.OutgoingPort,
						fmt.Sprintf("/tail/?app=%s", appId),
					)
					defer close(stop)

					logOutput := gbytes.NewBuffer()
					go func() {
						for message := range messages {
							Ω(message.GetSourceName()).To(Equal("STG"))
							logOutput.Write([]byte(string(message.GetMessage()) + "\n"))
						}
					}()

					err := suiteContext.NatsRunner.MessageBus.PublishWithReplyTo(
						"diego.staging.start",
						"stager-test",
						stagingMessage,
					)
					Ω(err).ShouldNot(HaveOccurred())

					var payload []byte
					Eventually(payloads, 10.0).Should(Receive(&payload))
					Ω(string(payload)).Should(Equal(`{"error":"Exited with status 1"}`))

					Eventually(logOutput).Should(gbytes.Say("no valid buildpacks detected"))
				})
			})
		})

		Context("with two stagers running", func() {
			BeforeEach(func() {
				suiteContext.StagerRunner.Start("--compilers", `{"default":"smelter.zip"}`)
				otherStagerRunner.Start("--compilers", `{"default":"smelter.zip"}`)
			})

			AfterEach(func() {
				otherStagerRunner.Stop()
			})

			It("only one returns a staging completed response", func() {
				received := make(chan bool)

				_, err := suiteContext.NatsRunner.MessageBus.Subscribe("two-stagers-test", func(message *yagnats.Message) {
					received <- true
				})
				Ω(err).ShouldNot(HaveOccurred())

				err = suiteContext.NatsRunner.MessageBus.PublishWithReplyTo(
					"diego.staging.start",
					"two-stagers-test",
					stagingMessage,
				)
				Ω(err).ShouldNot(HaveOccurred())

				Eventually(received, LONG_TIMEOUT).Should(Receive())
				Consistently(received, SHORT_TIMEOUT).ShouldNot(Receive())
			})
		})
	})
})

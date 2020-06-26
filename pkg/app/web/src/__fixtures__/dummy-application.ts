import { ApplicationKind } from "pipe/pkg/app/web/model/common_pb";
import { Application, ApplicationSyncStatus } from "../modules/applications";
import { dummyEnv } from "./dummy-environment";

export const dummyApplication: Application = {
  id: "application-1",
  cloudProvider: "",
  createdAt: 0,
  disabled: false,
  envId: dummyEnv.id,
  gitPath: { configPath: "", path: "", repoId: "repo-1" },
  kind: ApplicationKind.KUBERNETES,
  name: "DemoApp",
  pipedId: "piped-1",
  projectId: "project-1",
  mostRecentlySuccessfulDeployment: {
    deploymentId: "deployment-1",
    completedAt: 0,
    description: "",
    startedAt: 0,
    version: "v1",
  },
  mostRecentlyTriggeredDeployment: {
    deploymentId: "deployment-1",
    completedAt: 0,
    description: "",
    startedAt: 0,
    version: "v1",
  },
  syncState: {
    headDeploymentId: "deployment-1",
    reason: "",
    shortReason: "",
    status: ApplicationSyncStatus.SYNCED,
    timestamp: 0,
  },
  updatedAt: 0,
};
export interface ObjectMeta {
  name?: string;
  namespace?: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  creationTimestamp?: string;
  resourceVersion?: string;
}

export interface ListMeta {
  continue?: string;
  remainingItemCount?: number;
}

export interface ResourceList<T> {
  items?: T[];
  metadata?: ListMeta;
}

export interface BuildTarget {
  os?: string;
  arch?: string;
  buildFlag?: boolean;
  publishFlag?: boolean;
}

export interface GitRef {
  type?: "Branch" | "Tag" | "Commit";
  value?: string;
}

export interface PackageRepo {
  name?: string;
  url?: string;
  ref?: GitRef;
  buildTargets?: BuildTarget[];
}

export interface BootstrapRepo {
  name?: string;
  repo?: string;
}

export interface Project {
  apiVersion?: "ebs/v1";
  kind?: "Project";
  metadata?: ObjectMeta;
  spec?: {
    displayName?: string;
    description?: string;
    defaultRef?: { type?: "Branch" | "Tag"; value?: string };
    buildPayload?: string;
    buildTargets?: BuildTarget[];
    packageRepos?: PackageRepo[];
    bootstrapRepo?: BootstrapRepo[];
  };
  status?: { phase?: string };
}

export interface Snapshot {
  metadata?: ObjectMeta;
  status?: { phase?: string };
}

export interface Build {
  metadata?: ObjectMeta;
  spec?: { buildType?: string; packages?: string[]; buildTarget?: BuildTarget };
  status?: {
    phase?: string;
    stage?: string;
    startTime?: string;
    endTime?: string;
    repo?: string;
    baseBuildRef?: BootstrapRepo;
  };
}

export interface Job {
  metadata?: ObjectMeta;
  status?: { phase?: string; stage?: string; runner?: string; startTime?: string };
}

export interface SessionIdentity {
  type: "user" | "ops" | "admin" | "service";
  name: string;
  scopes: string[];
}

export interface Session {
  token: string;
  identity: SessionIdentity;
  expiresAt?: number;
}

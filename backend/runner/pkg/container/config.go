package container

type Config struct {
	Runtime    string
	Containerd ContainerdConfig
}

type ContainerdConfig struct {
	Address     string
	Namespace   string
	Snapshotter string
}

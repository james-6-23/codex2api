package version

// Version is injected by release builds. Development builds keep "dev".
var Version = "dev"

func Current() string {
	return Version
}

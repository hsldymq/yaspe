package fs

import (
	"github.com/pkg/errors"
	"path/filepath"
	"regexp"
	"strings"
)

// TODO

const (
	// separator is the directory separator, a slash
	separator = "/"

	// separatorChar is the directory separator as a character
	separatorChar = '/'

	// currentDir represents the current directory
	currentDir = "."
)

var (
	// windowsRootDirRegex matches Windows drive patterns like /C:/
	windowsRootDirRegex = regexp.MustCompile(`/\p{L}+:/`)
	// duplicateSlashesRegex matches duplicate consecutive slashes
	duplicateSlashesRegex = regexp.MustCompile(`/{2,}`)
)

// Path 代表本地文件系统中的文件或目录路径
type Path struct {
	raw            string
	path           string
	winDriveLetter string
}

// NewPathFromPathStr 从一个路径字符串创建一个新的 Path 结构体
func NewPathFromPathStr(pathString string) (*Path, error) {
	if pathString == "" {
		return nil, errors.New("cannot create a Path from an empty string")
	}

	normalizedPath := normalizePath(pathString)

	// Add a slash in front of paths with Windows drive letters
	if isWindowsFS(pathString, false) {
		pathString = "/" + pathString
	}

	return &Path{
		raw:  pathString,
		path: normalizedPath,
	}, nil
}

// NewFromParentChild creates a new Path by resolving a child path against a parent path
func NewFromParentChild(parent, child string) (*Path, error) {
	parentPath, err := NewPathFromPathStr(parent)
	if err != nil {
		return nil, err
	}
	childPath, err := NewPathFromPathStr(child)
	if err != nil {
		return nil, err
	}
	return parentPath.Resolve(childPath), nil
}

// NewFromParentChildPath creates a new Path by resolving a child Path against a parent string
func NewFromParentChildPath(parent string, child *Path) (*Path, error) {
	parentPath, err := NewPathFromPathStr(parent)
	if err != nil {
		return nil, err
	}
	return parentPath.Resolve(child), nil
}

// String returns the string representation of the path
func (p *Path) String() string {
	path := p.path
	// Remove leading slash for Windows drives when no scheme/authority
	if len(path) > 0 && path[0] == '/' && isWindowsFS(path, true) {
		path = path[1:]
	}
	return path
}

// GetPath 返回完整路径
func (p *Path) GetPath() string {
	return p.path
}

// IsAbsolute 返回该路径是否为绝对路径
func (p *Path) IsAbsolute() bool {
	start := 0
	if isWindowsFS(p.path, true) {
		start = 3
	}
	return strings.HasPrefix(p.path[start:], separator)
}

// GetName returns the final component of this path
func (p *Path) GetName() string {
	lastSlash := strings.LastIndex(p.path, separator)
	return p.path[lastSlash+1:]
}

// GetParent returns the parent of this path, or nil if at root
func (p *Path) GetParent() *Path {
	lastSlash := strings.LastIndex(p.path, "/")
	start := 0
	if isWindowsFS(p.path, true) {
		start = 3
	}

	// Check if we're at root or empty path
	if len(p.path) == start || (lastSlash == start && len(p.path) == start+1) {
		return nil
	}

	var parent string
	if lastSlash == -1 {
		parent = currentDir
	} else {
		end := start
		if isWindowsFS(p.path, true) {
			end = 3
		}
		if lastSlash == end {
			parent = p.path[:end+1]
		} else {
			parent = p.path[:lastSlash]
		}
	}

	result, _ := NewPathFromPathStr(parent) // We know this won't error since we're working with valid paths
	return result
}

// Suffix adds a suffix to the final name in the path
func (p *Path) Suffix(suffix string) *Path {
	parent := p.GetParent()
	newName := p.GetName() + suffix

	if parent == nil {
		result, _ := NewPathFromPathStr(newName)
		return result
	}

	return parent.Resolve(&Path{path: newName})
}

// Resolve resolves a child path against this parent path
func (p *Path) Resolve(child *Path) *Path {
	parentPath := p.path
	childPath := child.path

	// Ensure parent path ends with separator for proper resolution
	if !strings.HasSuffix(parentPath, "/") && parentPath != "/" && parentPath != "" {
		parentPath += "/"
	}

	// Remove leading separator from child if present
	if strings.HasPrefix(childPath, separator) {
		childPath = childPath[1:]
	}

	resolvedPath := parentPath + childPath
	result, _ := NewPathFromPathStr(resolvedPath) // We know this won't error
	return result
}

// Depth returns the number of elements in this path
func (p *Path) Depth() int {
	path := p.path
	depth := 0
	slash := 0
	if len(path) == 1 && path[0] == '/' {
		slash = -1
	}

	for slash != -1 {
		depth++
		slash = strings.Index(path[slash+1:], separator)
		if slash != -1 {
			slash += len(path[:slash+1]) - len(path[slash+1:])
		}
	}
	return depth
}

// HasWindowsDrive returns true if this path contains a Windows drive letter
func (p *Path) HasWindowsDrive() bool {
	return isWindowsFS(p.path, true)
}

// Equals checks if two paths are equal
func (p *Path) Equals(other *Path) bool {
	if other == nil {
		return false
	}
	return p.path == other.path
}

// ToNativePath converts the path to the native OS path format
func (p *Path) ToNativePath() string {
	nativePath := p.String()
	return filepath.FromSlash(nativePath)
}

// FromNativePath creates a Path from a native OS path
func FromNativePath(nativePath string) (*Path, error) {
	unixPath := filepath.ToSlash(nativePath)
	return NewPathFromPathStr(unixPath)
}

// normalizePath normalizes a path string
func normalizePath(path string) string {
	// Replace backslashes with forward slashes
	path = strings.ReplaceAll(path, "\\", "/")

	// Remove duplicate consecutive slashes
	if strings.Contains(path, "//") {
		path = duplicateSlashesRegex.ReplaceAllString(path, "/")
	}

	// Remove trailing separator, except for root paths
	if strings.HasSuffix(path, separator) &&
		path != separator && // UNIX root path
		!windowsRootDirRegex.MatchString(path) { // Windows root path
		path = path[:len(path)-len(separator)]
	}

	return path
}

// isWindowsFS 检查提供的路径是否属于Windows文件系统,即路径开头携带盘符
func isWindowsFS(path string, slashed bool) bool {
	start := 0
	if slashed {
		start = 1
	}

	return len(path) >= start+2 &&
		(!slashed || path[0] == '/') &&
		path[start+1] == ':' &&
		((path[start] >= 'A' && path[start] <= 'Z') ||
			(path[start] >= 'a' && path[start] <= 'z'))
}

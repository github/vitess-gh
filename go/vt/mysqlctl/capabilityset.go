/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
 Detect server flavors and capabilities
*/

package mysqlctl

type MySQLFlavor string

// Flavor constants define the type of mysql flavor being used
const (
	FlavorMySQL   MySQLFlavor = "mysql"
	FlavorPercona MySQLFlavor = "percona"
	FlavorMariaDB MySQLFlavor = "mariadb"
)

// Mysqld is the object that represents a mysqld daemon running on this server.
type capabilitySet struct {
	flavor  MySQLFlavor
	version ServerVersion
}

func newCapabilitySet(f MySQLFlavor, v ServerVersion) (c capabilitySet) {
	c.flavor = f
	c.version = v
	return
}

func (c *capabilitySet) hasMySQLUpgradeInServer() bool {
	return c.isMySQLLike() && c.version.atLeast(ServerVersion{Major: 8, Minor: 0, Patch: 16})
}
func (c *capabilitySet) hasInitializeInServer() bool {
	return c.isMySQLLike() && c.version.atLeast(ServerVersion{Major: 5, Minor: 7, Patch: 0})
}
func (c *capabilitySet) hasMaria104InstallDb() bool {
	return c.isMariaDB() && c.version.atLeast(ServerVersion{Major: 10, Minor: 4, Patch: 0})
}

// IsMySQLLike tests if the server is either MySQL
// or Percona Server. At least currently, Vitess doesn't
// make use of any specific Percona Server features.
func (c *capabilitySet) isMySQLLike() bool {
	return c.flavor == FlavorMySQL || c.flavor == FlavorPercona
}
func (c *capabilitySet) isMariaDB() bool {
	return c.flavor == FlavorMariaDB
}

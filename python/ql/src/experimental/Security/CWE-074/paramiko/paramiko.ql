/**
 * @name RCE with user provided command with paramiko ssh client
 * @description user provided command can lead to execute code on a external server that can be belong to other users or admins
 * @kind path-problem
 * @problem.severity error
 * @security-severity 9.3
 * @precision high
 * @id py/command-injection
 * @tags security
 *       experimental
 *       external/cwe/cwe-074
 */

import python
import semmle.python.dataflow.new.DataFlow
import semmle.python.dataflow.new.TaintTracking
import semmle.python.dataflow.new.RemoteFlowSources
import semmle.python.ApiGraphs
import DataFlow::PathGraph

private API::Node paramikoClient() {
  result = API::moduleImport("paramiko").getMember("SSHClient").getReturn()
}

class ParamikoCmdInjectionConfiguration extends TaintTracking::Configuration {
  ParamikoCmdInjectionConfiguration() { this = "ParamikoCMDInjectionConfiguration" }

  override predicate isSource(DataFlow::Node source) { source instanceof RemoteFlowSource }

  /**
   * exec_command of `paramiko.SSHClient` class execute command on ssh target server
   * the `paramiko.ProxyCommand` is equivalent of `ssh -o ProxyCommand="CMD"`
   *  and it run CMD on current system that running the ssh command
   * the Sink related to proxy command is the `connect` method of `paramiko.SSHClient` class
   */
  override predicate isSink(DataFlow::Node sink) {
    sink = paramikoClient().getMember("exec_command").getACall().getParameter(0, "command").asSink()
    or
    sink = paramikoClient().getMember("connect").getACall().getParameter(11, "sock").asSink()
  }

  /**
   * this additional taint step help taint tracking to find the vulnerable `connect` method of `paramiko.SSHClient` class
   */
  override predicate isAdditionalTaintStep(DataFlow::Node nodeFrom, DataFlow::Node nodeTo) {
    exists(API::CallNode call |
      call = API::moduleImport("paramiko").getMember("ProxyCommand").getACall() and
      nodeFrom = call.getParameter(0, "command_line").asSink() and
      nodeTo = call
    )
  }
}

from ParamikoCmdInjectionConfiguration config, DataFlow::PathNode source, DataFlow::PathNode sink
where config.hasFlowPath(source, sink)
select sink.getNode(), source, sink, "This code execution depends on a $@.", source.getNode(),
  "a user-provided value"

/**
 * Provides a taint-tracking configuration for reasoning about SQL injection vulnerabilities.
 */

import csharp
private import semmle.code.csharp.security.dataflow.flowsources.Remote
private import semmle.code.csharp.security.dataflow.flowsources.Local
private import semmle.code.csharp.frameworks.Sql
private import semmle.code.csharp.security.Sanitizers
private import semmle.code.csharp.dataflow.ExternalFlow

/**
 * A source specific to SQL injection vulnerabilities.
 */
abstract class Source extends DataFlow::Node { }

/**
 * A sink for SQL injection vulnerabilities.
 */
abstract class Sink extends DataFlow::ExprNode { }

/**
 * A sanitizer for SQL injection vulnerabilities.
 */
abstract class Sanitizer extends DataFlow::ExprNode { }

/**
 * DEPRECATED: Use `SqlInjection` instead.
 *
 * A taint-tracking configuration for SQL injection vulnerabilities.
 */
deprecated class TaintTrackingConfiguration extends TaintTracking::Configuration {
  TaintTrackingConfiguration() { this = "SqlInjection" }

  override predicate isSource(DataFlow::Node source) { source instanceof Source }

  override predicate isSink(DataFlow::Node sink) { sink instanceof Sink }

  override predicate isSanitizer(DataFlow::Node node) { node instanceof Sanitizer }
}

/**
 * A taint-tracking configuration for SQL injection vulnerabilities.
 */
module SqlInjectionConfig implements DataFlow::ConfigSig {
  /**
   * Holds if `source` is a relevant data flow source.
   */
  predicate isSource(DataFlow::Node source) { source instanceof Source }

  /**
   * Holds if `sink` is a relevant data flow sink.
   */
  predicate isSink(DataFlow::Node sink) { sink instanceof Sink }

  /**
   * Holds if data flow through `node` is prohibited. This completely removes
   * `node` from the data flow graph.
   */
  predicate isBarrier(DataFlow::Node node) { node instanceof Sanitizer }
}

/**
 * A taint-tracking module for SQL injection vulnerabilities.
 */
module SqlInjection = TaintTracking::Global<SqlInjectionConfig>;

/** A source of remote user input. */
class RemoteSource extends Source instanceof RemoteFlowSource { }

/** A source of local user input. */
class LocalSource extends Source instanceof LocalFlowSource { }

/** An SQL expression passed to an API call that executes SQL. */
class SqlInjectionExprSink extends Sink {
  SqlInjectionExprSink() { exists(SqlExpr s | this.getExpr() = s.getSql()) }
}

/** SQL sinks defined through CSV models. */
private class ExternalSqlInjectionExprSink extends Sink {
  ExternalSqlInjectionExprSink() { sinkNode(this, "sql") }
}

private class SimpleTypeSanitizer extends Sanitizer, SimpleTypeSanitizedExpr { }

private class GuidSanitizer extends Sanitizer, GuidSanitizedExpr { }

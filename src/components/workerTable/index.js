import React, { Component } from 'react';
import { Layout, Menu, Icon, Table, Modal } from 'antd'
class WorkerTable extends Component {
  render() {
    return (
      <Layout style={{ minHeight: '100vh'}} >
        <iframe style={{ minHeight: '100vh'}} src="http://localhost:5040/#/processes" />
      </Layout>
    )
  }
}

export default WorkerTable
